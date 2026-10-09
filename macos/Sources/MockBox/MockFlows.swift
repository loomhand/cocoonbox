import CocoonBoxKit
import CryptoKit
import Foundation
import GRPCCore

// MockFlows is the user's flow library as the api keeps it (contracts.md "Flow library"): last write wins, a write of
// the same content keeps its rev, and names are .json or .mts file names.
actor MockFlows {
    private var rev: UInt64 = 0
    private var flows: [String: Cocoonbox_Control_V1_Flow] = [:]

    func list() -> Cocoonbox_Control_V1_ListFlowsResponse {
        let entries = flows.values.sorted { $0.rev > $1.rev }.map { flow in
            var entry = flow
            entry.content = Data()
            return entry
        }
        return .with {
            $0.rev = rev
            $0.flows = entries
        }
    }

    func export(_ names: [String]) throws -> [Cocoonbox_Control_V1_Flow] {
        let wanted = names.isEmpty ? flows.values.sorted { $0.rev > $1.rev }.map(\.name) : names
        return try wanted.map { name in
            guard let flow = flows[name] else { throw RPCError(code: .notFound, message: "\(name): flow not found") }
            return flow
        }
    }

    func save(_ items: [Cocoonbox_Control_V1_Flow]) throws -> [Cocoonbox_Control_V1_Flow] {
        for item in items where !Self.valid(item.name) || String(data: item.content, encoding: .utf8) == nil {
            throw RPCError(code: .invalidArgument, message: "invalid flow: \(item.name)")
        }
        return items.map { item in
            let sha = SHA256.hash(data: item.content).map { String(format: "%02x", $0) }.joined()
            if let stored = flows[item.name], stored.sha256 == sha { return stored.withoutContent }
            rev += 1
            let flow = Cocoonbox_Control_V1_Flow.with {
                $0.name = item.name
                $0.content = item.content
                $0.rev = rev
                $0.sha256 = sha
                $0.size = UInt64(item.content.count)
                $0.savedAtMs = Int64(Date().timeIntervalSince1970 * 1000)
                $0.image = item.image
            }
            flows[item.name] = flow
            return flow.withoutContent
        }
    }

    func delete(_ names: [String]) -> [String] {
        let deleted = names.filter { flows.removeValue(forKey: $0) != nil }
        if !deleted.isEmpty { rev += 1 }
        return deleted
    }

    static func valid(_ name: String) -> Bool {
        name.range(of: #"^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$"#, options: .regularExpression) != nil
            && (name.hasSuffix(".json") || name.hasSuffix(".mts"))
    }
}

extension Cocoonbox_Control_V1_Flow {
    var withoutContent: Self {
        var flow = self
        flow.content = Data()
        return flow
    }
}

struct FlowsService: Cocoonbox_Control_V1_Flows.SimpleServiceProtocol {
    let flows: MockFlows

    func list(request: Cocoonbox_Control_V1_ListFlowsRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_ListFlowsResponse {
        await flows.list()
    }

    func export(request: Cocoonbox_Control_V1_ExportFlowsRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_ExportFlowsResponse {
        let exported = try await flows.export(request.names)
        return .with { $0.flows = exported }
    }

    func save(request: Cocoonbox_Control_V1_SaveFlowsRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_SaveFlowsResponse {
        let stored = try await flows.save(request.flows)
        return .with { $0.flows = stored }
    }

    func delete(request: Cocoonbox_Control_V1_DeleteFlowsRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_DeleteFlowsResponse {
        let deleted = await flows.delete(request.names)
        return .with { $0.deleted = deleted }
    }
}
