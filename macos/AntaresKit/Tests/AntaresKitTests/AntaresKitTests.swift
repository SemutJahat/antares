import XCTest
@testable import AntaresKit

final class AntaresKitTests: XCTestCase {
    func testDefaultBaseURLIsLoopback8787() {
        XCTAssertEqual(AntaresKit.defaultBaseURL.absoluteString, "http://127.0.0.1:8787")
    }
}
