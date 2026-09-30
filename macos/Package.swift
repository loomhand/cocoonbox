// swift-tools-version: 6.0

import PackageDescription

// Command Line Tools ship swift-testing outside the default search path and
// without the _Testing_Foundation overlay module, and `swift test` does not
// find it there, so the tests build as an executable run by scripts/test.sh.
let testingFrameworks = "/Library/Developer/CommandLineTools/Library/Developer/Frameworks"

let package = Package(
    name: "CocoonBox",
    platforms: [.macOS(.v15)],
    products: [
        .library(name: "CocoonBoxKit", targets: ["CocoonBoxKit"]),
        .executable(name: "CocoonBox", targets: ["CocoonBox"]),
        .executable(name: "MockBox", targets: ["MockBox"]),
    ],
    dependencies: [
        .package(url: "https://github.com/grpc/grpc-swift-2.git", from: "2.4.3"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", from: "2.10.0"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", from: "2.4.1"),
        .package(url: "https://github.com/apple/swift-protobuf.git", from: "1.38.1"),
        // 1.1.0, pinned by revision because it depends on a CryptoSwift branch
        .package(url: "https://github.com/royalapplications/royalvnc.git", revision: "92d4427c73817d8f849bb289ff190aa4b40c44ea"),
    ],
    targets: [
        .target(
            name: "CocoonBoxKit",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCNIOTransportHTTP2TransportServices", package: "grpc-swift-nio-transport"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
                .product(name: "RoyalVNCKit", package: "royalvnc"),
            ]
        ),
        .target(name: "RFBTestServer"),
        .executableTarget(
            name: "CocoonBox",
            dependencies: ["CocoonBoxKit"]
        ),
        .executableTarget(
            name: "MockBox",
            dependencies: ["CocoonBoxKit", "RFBTestServer"]
        ),
        .executableTarget(
            name: "CocoonBoxKitTests",
            dependencies: [
                "CocoonBoxKit",
                "RFBTestServer",
                .product(name: "RoyalVNCKit", package: "royalvnc"),
            ],
            path: "Tests/CocoonBoxKitTests",
            swiftSettings: [.unsafeFlags(["-F", testingFrameworks, "-Xfrontend", "-disable-cross-import-overlays"])],
            linkerSettings: [.unsafeFlags(["-F", testingFrameworks, "-Xlinker", "-rpath", "-Xlinker", testingFrameworks])]
        ),
    ]
)
