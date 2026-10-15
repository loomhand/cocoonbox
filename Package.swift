// swift-tools-version: 6.0

import PackageDescription

// The macOS client's libraries and CLI for packages that build on them; the app, MockBox and the tests build from macos/Package.swift.
let package = Package(
    name: "cocoonbox",
    platforms: [.macOS(.v15)],
    products: [
        .library(name: "CocoonBoxKit", targets: ["CocoonBoxKit"]),
        .library(name: "CocoonBoxTerminal", targets: ["CocoonBoxTerminal"]),
        .executable(name: "cocoonbox-cli", targets: ["CocoonBoxCLI"]),
    ],
    dependencies: [
        .package(url: "https://github.com/grpc/grpc-swift-2.git", from: "2.4.3"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", from: "2.10.0"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", from: "2.4.1"),
        .package(url: "https://github.com/apple/swift-protobuf.git", from: "1.38.1"),
        .package(url: "https://github.com/loomhand/swift-rfb.git", from: "0.1.0"),
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
            ],
            path: "macos/Sources/CocoonBoxKit"
        ),
        .target(
            name: "CocoonBoxTerminal",
            dependencies: [.product(name: "GhosttyTerminal", package: "libghostty-spm")],
            path: "macos/Sources/CocoonBoxTerminal"
        ),
        .target(
            name: "CocoonBoxBridge",
            dependencies: ["CocoonBoxKit"],
            path: "macos/Sources/CocoonBoxBridge"
        ),
        .executableTarget(
            name: "CocoonBoxCLI",
            dependencies: ["CocoonBoxKit", "CocoonBoxBridge"],
            path: "macos/Sources/CocoonBoxCLI"
        ),
    ]
)
