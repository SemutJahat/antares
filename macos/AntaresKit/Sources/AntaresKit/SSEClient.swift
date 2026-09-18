import Foundation

public enum SSEParser {
    public static func parse(_ data: Data, onEvent: ([String: String]) throws -> Void) throws {
        let bytes = Array(data)
        var dataLines: [String] = []
        var lineStart = 0
        var i = 0

        func flushLine(_ end: Int) throws {
            let line = String(decoding: bytes[lineStart..<end], as: UTF8.self)
            if line.hasPrefix(":") { return }
            if line.isEmpty {
                let payload = dataLines.joined(separator: "\n")
                dataLines = []
                guard !payload.isEmpty, payload != "[DONE]" else { return }
                let obj = try JSONSerialization.jsonObject(with: Data(payload.utf8))
                guard let dict = obj as? [String: Any] else { return }
                var strings: [String: String] = [:]
                for (k, v) in dict {
                    strings[k] = (v as? String) ?? "\(v)"
                }
                try onEvent(strings)
                return
            }
            if line.hasPrefix("data:") {
                var v = String(line.dropFirst(5))
                if v.first == " " { v.removeFirst() }
                dataLines.append(v)
            }
        }

        while i < bytes.count {
            if bytes[i] == 0x0A {
                try flushLine(i)
                i += 1
                lineStart = i
                continue
            }
            if bytes[i] == 0x0D {
                try flushLine(i)
                if i + 1 < bytes.count, bytes[i + 1] == 0x0A {
                    i += 2
                } else {
                    i += 1
                }
                lineStart = i
                continue
            }
            i += 1
        }
    }
}
