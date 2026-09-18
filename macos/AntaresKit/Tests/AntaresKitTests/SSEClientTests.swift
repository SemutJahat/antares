import XCTest
@testable import AntaresKit

final class SSEClientTests: XCTestCase {
    func testDispatchesJSONOnBlankLineAndStripsLeadingSpace() throws {
        let raw = "data: {\"delta\":\"Hel\"}\n\ndata: {\"delta\":\"lo\"}\r\n\r\n"
        var events: [[String: String]] = []
        try SSEParser.parse(Data(raw.utf8)) { json in
            events.append(json)
        }
        XCTAssertEqual(events, [["delta": "Hel"], ["delta": "lo"]])
    }

    func testIgnoresCommentsAndDoneSentinel() throws {
        let raw = ": keep-alive\n\ndata: [DONE]\n\ndata: {\"ok\":true}\n\n"
        var count = 0
        try SSEParser.parse(Data(raw.utf8)) { _ in count += 1 }
        XCTAssertEqual(count, 1)
    }

    func testDropsUnfinishedTrailingRecord() throws {
        let raw = "data: {\"a\":1}\n\ndata: {\"b\":2}"
        var count = 0
        try SSEParser.parse(Data(raw.utf8)) { _ in count += 1 }
        XCTAssertEqual(count, 1)
    }
}
