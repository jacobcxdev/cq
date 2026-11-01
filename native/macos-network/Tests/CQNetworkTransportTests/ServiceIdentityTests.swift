import CQNetworkIPC
import XCTest

final class ServiceIdentityTests: XCTestCase {
    func testBuildsUseSeparateServiceRegistrations() throws {
        let first = try CQNetworkIdentity.machService(buildVersion: "6")
        let next = try CQNetworkIdentity.machService(buildVersion: "7")
        XCTAssertEqual(first, "K2648T24P4.dev.jacobcx.cq.network.proxy.6")
        XCTAssertNotEqual(first, next)
        XCTAssertEqual(try CQNetworkIdentity.machService(buildVersion: "1.2.3"),
                       "K2648T24P4.dev.jacobcx.cq.network.proxy.1.2.3")
    }

    func testInvalidBuildCannotSelectUnversionedOrForeignService() {
        for value in [nil, "", "6.", ".6", "1..2", "1.2.3.4", "6\n", "../other", "６"] {
            XCTAssertThrowsError(try CQNetworkIdentity.machService(buildVersion: value))
        }
    }
}
