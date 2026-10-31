// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "CQNetwork",
    platforms: [.macOS(.v13)],
    products: [
        .library(name: "CQNetwork", targets: ["CQNetwork"]),
        .library(name: "CQNetworkIPC", targets: ["CQNetworkIPC"]),
        .library(name: "CQNetworkTransport", targets: ["CQNetworkTransport"]),
        .executable(name: "cq-network", targets: ["CQNetworkHost"]),
    ],
    targets: [
        .target(name: "CQNetwork"),
        .target(name: "CQNetworkIPC"),
        .target(name: "CQNetworkTransport", dependencies: ["CQNetwork", "CQNetworkIPC"]),
        .executableTarget(name: "CQNetworkHost", dependencies: ["CQNetworkIPC", "CQNetworkTransport"]),
        .testTarget(name: "CQNetworkTests", dependencies: ["CQNetwork"]),
        .testTarget(name: "CQNetworkTransportTests", dependencies: ["CQNetwork", "CQNetworkIPC", "CQNetworkTransport"]),
    ],
    swiftLanguageModes: [.v5]
)
