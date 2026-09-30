import CocoonBoxKit
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2TransportServices

// MockBox serves Boxes, Agents and Session over plaintext HTTP/2 with canned
// agent turns and a synthetic RFB desktop, for running the app without a box.
//   MockBox [--port 50051] [--token mock-token] [--boot-ms 2000] [--animate FPS]

var options: [String: String] = [:]
var args = CommandLine.arguments.dropFirst().makeIterator()
while let key = args.next() {
    options[key] = args.next()
}
let port = Int(options["--port"] ?? "") ?? 50051
let token = options["--token"] ?? "mock-token"
let boot = Int(options["--boot-ms"] ?? "") ?? 2000
let animate = Int(options["--animate"] ?? "") ?? 0

let world = MockWorld(token: token, bootDelay: .milliseconds(boot), animateFPS: animate)
let boxID = await world.box.id
let transport = HTTP2ServerTransport.TransportServices(
    address: .ipv4(host: "127.0.0.1", port: port),
    transportSecurity: .plaintext
)
let server = GRPCServer(
    transport: transport,
    services: [BoxesService(world: world), AgentsService(world: world), SessionService(world: world)],
    interceptors: [AuthCheck(token: token, boxID: boxID)]
)
print("mockbox listening on http://127.0.0.1:\(port) token=\(token) box=\(boxID)")
try await server.serve()
