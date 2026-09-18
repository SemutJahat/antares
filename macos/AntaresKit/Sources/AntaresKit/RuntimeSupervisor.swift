import Darwin
import Foundation

public enum RuntimePlan {
    public enum Mode: Equatable { case attached, spawned, conflict }

    public static func decide(healthOK: Bool, portBusy: Bool) -> Mode {
        if healthOK { return .attached }
        if portBusy { return .conflict }
        return .spawned
    }

    public static func shouldStopChild(mode: Mode) -> Bool { mode == .spawned }
}

public struct HealthResponse: Decodable, Sendable {
    public let ok: Bool
    public let version: String
}

public enum RuntimeSupervisorError: Error, LocalizedError {
    case missingBinary
    case portConflict
    case healthTimeout
    case spawnFailed(String)

    public var errorDescription: String? {
        switch self {
        case .missingBinary: "Bundled antares binary not found"
        case .portConflict: "Port in use by a process that is not Antares"
        case .healthTimeout: "Backend did not become healthy within 30 seconds"
        case .spawnFailed(let detail): detail
        }
    }
}

/// Captures stderr lines from a spawned child for LaunchView.
public actor StderrLog {
    private var lines: [String] = []

    public var lastLog: String { lines.last ?? "" }

    func append(_ data: Data) {
        let chunk = String(decoding: data, as: UTF8.self)
        for line in chunk.split(whereSeparator: \.isNewline) {
            let text = String(line)
            if !text.isEmpty { lines.append(text) }
        }
    }
}

public final class RuntimeSupervisor: @unchecked Sendable {
    public private(set) var mode: RuntimePlan.Mode?
    public let stderrLog = StderrLog()

    private let baseURL: URL
    private let client: APIClient
    private var process: Process?
    private var stderrTask: Task<Void, Never>?

    public init(baseURL: URL? = nil, session: URLSession = .shared) {
        let resolved = baseURL
            ?? ProcessInfo.processInfo.environment["ANTARES_URL"].flatMap { URL(string: $0) }
            ?? AntaresKit.defaultBaseURL
        self.baseURL = resolved
        self.client = APIClient(baseURL: resolved, session: session)
    }

    /// Probes health, attaches or spawns the bundled backend, then waits until healthy.
    public func start(binary: URL?) async throws {
        let healthOK = await probeHealth()
        let portBusy = !healthOK && isPortBusy(baseURL: baseURL)
        let plan = RuntimePlan.decide(healthOK: healthOK, portBusy: portBusy)
        mode = plan

        switch plan {
        case .attached:
            return
        case .conflict:
            throw RuntimeSupervisorError.portConflict
        case .spawned:
            guard let binary else { throw RuntimeSupervisorError.missingBinary }
            try spawn(binary: binary)
            try await waitForHealth()
        }
    }

    /// Stops a spawned child with SIGTERM, then SIGKILL after 5s. Attached mode is left running.
    public func stop() async {
        guard let mode, RuntimePlan.shouldStopChild(mode: mode) else { return }
        stderrTask?.cancel()
        stderrTask = nil
        guard let process else { return }
        if process.isRunning {
            process.terminate()
            let pid = process.processIdentifier
            try? await Task.sleep(for: .seconds(5))
            if process.isRunning {
                kill(pid, SIGKILL)
            }
        }
        self.process = nil
    }

    private func probeHealth() async -> Bool {
        (try? await fetchHealth())?.ok == true
    }

    private func fetchHealth() async throws -> HealthResponse {
        try await client.get("/api/health")
    }

    private func waitForHealth() async throws {
        let deadline = Date().addingTimeInterval(30)
        while Date() < deadline {
            if await probeHealth() { return }
            try await Task.sleep(for: .milliseconds(200))
        }
        let detail = await stderrLog.lastLog
        throw RuntimeSupervisorError.spawnFailed(
            detail.isEmpty ? RuntimeSupervisorError.healthTimeout.localizedDescription : detail
        )
    }

    private func spawn(binary: URL) throws {
        let child = Process()
        child.executableURL = binary
        child.arguments = ["--foreground"]
        var env = ProcessInfo.processInfo.environment
        env["HOME"] = NSHomeDirectory()
        child.environment = env

        let pipe = Pipe()
        child.standardOutput = FileHandle.nullDevice
        child.standardError = pipe

        try child.run()
        process = child
        stderrTask = Task { [stderrLog] in
            let handle = pipe.fileHandleForReading
            while !Task.isCancelled {
                let data = handle.availableData
                if data.isEmpty { break }
                await stderrLog.append(data)
            }
        }
    }

    private func isPortBusy(baseURL: URL) -> Bool {
        guard let host = baseURL.host else { return false }
        let port = baseURL.port ?? (baseURL.scheme == "https" ? 443 : 80)
        var hints = addrinfo(
            ai_flags: AI_NUMERICHOST,
            ai_family: AF_UNSPEC,
            ai_socktype: SOCK_STREAM,
            ai_protocol: IPPROTO_TCP,
            ai_addrlen: 0,
            ai_canonname: nil,
            ai_addr: nil,
            ai_next: nil
        )
        var result: UnsafeMutablePointer<addrinfo>?
        defer { if result != nil { freeaddrinfo(result) } }
        guard getaddrinfo(host, String(port), &hints, &result) == 0, let info = result else {
            return false
        }
        let fd = socket(info.pointee.ai_family, info.pointee.ai_socktype, info.pointee.ai_protocol)
        guard fd >= 0 else { return false }
        defer { close(fd) }
        var addr = info.pointee.ai_addr.withMemoryRebound(to: sockaddr.self, capacity: 1) { $0.pointee }
        let len = info.pointee.ai_addrlen
        return withUnsafePointer(to: &addr) { ptr in
            connect(fd, UnsafeRawPointer(ptr).assumingMemoryBound(to: sockaddr.self), len) == 0
        }
    }
}
