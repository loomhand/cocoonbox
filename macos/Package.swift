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
        .library(name: "CocoonBoxTerminal", targets: ["CocoonBoxTerminal"]),
        .executable(name: "CocoonBox", targets: ["CocoonBox"]),
        .executable(name: "MockBox", targets: ["MockBox"]),
    ],
    dependencies: [
        .package(url: "https://github.com/grpc/grpc-swift-2.git", from: "2.4.3"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", from: "2.10.0"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", from: "2.4.1"),
        .package(url: "https://github.com/apple/swift-protobuf.git", from: "1.38.1"),
        .package(path: "../../swift-rfb"),
        .package(url: "https://github.com/Lakr233/libghostty-spm.git", exact: "1.6.20260929"),
    ],
    targets: [
        .target(
            name: "CocoonBoxKit",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCNIOTransportHTTP2TransportServices", package: "grpc-swift-nio-transport"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
                .product(name: "RFB", package: "swift-rfb"),
                .product(name: "RFBMetal", package: "swift-rfb"),
            ]
        ),
        .target(
            name: "CocoonBoxTerminal",
            dependencies: [.product(name: "GhosttyTerminal", package: "libghostty-spm")]
        ),
        .target(name: "RFBTestServer"),
        .executableTarget(
            name: "CocoonBox",
            dependencies: ["CocoonBoxKit", "CocoonBoxTerminal"]
        ),
        .executableTarget(
            name: "MockBox",
            dependencies: ["CocoonBoxKit", "RFBTestServer"]
        ),
        .executableTarget(
            name: "CocoonBoxKitTests",
            dependencies: [
                "CocoonBoxKit",
                "CocoonBoxTerminal",
                "RFBTestServer",
                .product(name: "RFB", package: "swift-rfb"),
            ],
            path: "Tests/CocoonBoxKitTests",
            swiftSettings: [.unsafeFlags(["-F", testingFrameworks, "-Xfrontend", "-disable-cross-import-overlays"])],
            linkerSettings: [.unsafeFlags(["-F", testingFrameworks, "-Xlinker", "-rpath", "-Xlinker", testingFrameworks])]
        ),
    ]
)
