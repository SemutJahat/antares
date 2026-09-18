import XCTest
@testable import AntaresKit

final class RuntimeSupervisorTests: XCTestCase {
    func testHealthyServerMeansAttach() {
        XCTAssertEqual(RuntimePlan.decide(healthOK: true, portBusy: false), .attached)
    }

    func testUnhealthyAndFreePortMeansSpawn() {
        XCTAssertEqual(RuntimePlan.decide(healthOK: false, portBusy: false), .spawned)
    }

    func testUnhealthyBusyPortIsError() {
        XCTAssertEqual(RuntimePlan.decide(healthOK: false, portBusy: true), .conflict)
    }

    func testSpawnedChildIsStoppedOnShutdownAttachedIsNot() {
        XCTAssertTrue(RuntimePlan.shouldStopChild(mode: .spawned))
        XCTAssertFalse(RuntimePlan.shouldStopChild(mode: .attached))
    }
}
