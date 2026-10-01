import CocoonBoxKit
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2TransportServices

// MockBox serves Boxes, Agents and Session over plaintext HTTP/2, and MCP at
// /cocoonbox.mcp/<box id> on the same port, with canned agent turns and a
// synthetic RFB desktop, for running the app without a box.
//   MockBox [--port 50051] [--token mock-token] [--boot-ms 2000] [--animate FPS] [--external-ms MS] [--external-history 1] [--restore-on-wake 1] [--credentials 0]
// --external-ms starts one scripted MCP session (agent mcp:claude-code) MS after the box runs.
// --external-history 1 adds idle MCP sessions from earlier hours.
// --restore-on-wake 1 wakes the box as the api does a box without a machine: restored into a new one.
// --credentials 0 answers ActiveOrigin and FillCredential with UNIMPLEMENTED, as an agent before v12 does.

setvbuf(stdout, nil, _IOLBF, 0)

var options: [String: String] = [:]
var args = CommandLine.arguments.dropFirst().makeIterator()
while let key = args.next() {
    options[key] = args.next()
}
let port = Int(options["--port"] ?? "") ?? 50051
let token = options["--token"] ?? "mock-token"
let boot = Int(options["--boot-ms"] ?? "") ?? 2000
let animate = Int(options["--animate"] ?? "") ?? 0
let external = Int(options["--external-ms"] ?? "").map { Duration.milliseconds($0) }

let world = MockWorld(token: token, bootDelay: .milliseconds(boot), animateFPS: animate, externalDelay: external, externalHistory: options["--external-history"] == "1", restoreOnWake: options["--restore-on-wake"] == "1")
let boxID = await world.box.id
let transport = HTTP2ServerTransport.TransportServices(
    address: .ipv4(host: "127.0.0.1", port: 0),
    transportSecurity: .plaintext
)
let server = GRPCServer(
    transport: transport,
    services: [BoxesService(world: world), AgentsService(world: world), SessionService(world: world, credentials: options["--credentials"] != "0")],
    interceptors: [AuthCheck(token: token, boxID: boxID)]
)
try await withThrowingTaskGroup(of: Void.self) { group in
    group.addTask { try await server.serve() }
    guard let grpc = try await transport.listeningAddress.ipv4 else { throw CancellationError() }
    try FrontDoor(grpcPort: UInt16(grpc.port), mcp: MCPServer(world: world, token: token, boxID: boxID)).start(port: UInt16(port))
    print("mockbox listening on http://127.0.0.1:\(port) token=\(token) box=\(boxID) mcp=/cocoonbox.mcp/\(boxID)")
    try await group.next()
}
