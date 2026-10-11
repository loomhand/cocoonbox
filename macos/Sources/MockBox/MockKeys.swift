import CocoonBoxKit
import Foundation
import GRPCCore

// MockKeys is the user's key vault as the api keeps it: a value goes in and never comes back, and a rotate keeps the placeholder.
actor MockKeys {
    private var keys: [String: Cocoonbox_Control_V1_Key] = [:]

    func add(_ request: Cocoonbox_Control_V1_AddKeyRequest) throws -> Cocoonbox_Control_V1_Key {
        guard !request.name.isEmpty, !request.hosts.isEmpty, !request.value.isEmpty else {
            throw RPCError(code: .invalidArgument, message: "a key needs a name, hosts and a value")
        }
        guard keys[request.name] == nil else { throw RPCError(code: .alreadyExists, message: "a key is already named \(request.name)") }
        let now = Int64(Date().timeIntervalSince1970 * 1000)
        let key = Cocoonbox_Control_V1_Key.with {
            $0.name = request.name
            $0.hosts = request.hosts
            $0.query = request.query
            $0.header = request.query.isEmpty ? (request.header.isEmpty ? "Authorization" : request.header) : ""
            $0.template = request.query.isEmpty ? (request.template.isEmpty ? "Bearer {}" : request.template) : ""
            $0.placeholder = "cbv_" + UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased()
            $0.hint = Self.hint(request.value)
            $0.createdAtMs = now
            $0.updatedAtMs = now
        }
        keys[request.name] = key
        return key
    }

    func list() -> [Cocoonbox_Control_V1_Key] {
        keys.values.sorted { $0.name < $1.name }
    }

    func rotate(_ name: String, _ value: String) throws -> Cocoonbox_Control_V1_Key {
        guard var key = keys[name] else { throw RPCError(code: .notFound, message: "no such key") }
        key.hint = Self.hint(value)
        key.updatedAtMs = Int64(Date().timeIntervalSince1970 * 1000)
        keys[name] = key
        return key
    }

    func delete(_ name: String) throws {
        guard keys.removeValue(forKey: name) != nil else { throw RPCError(code: .notFound, message: "no such key") }
    }

    static func hint(_ value: String) -> String {
        value.count >= 12 ? String(value.suffix(4)) : ""
    }
}

struct KeysService: Cocoonbox_Control_V1_Keys.SimpleServiceProtocol {
    let keys: MockKeys

    func add(request: Cocoonbox_Control_V1_AddKeyRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_Key {
        try await keys.add(request)
    }

    func list(request: Cocoonbox_Control_V1_ListKeysRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_ListKeysResponse {
        let all = await keys.list()
        return .with { $0.keys = all }
    }

    func rotate(request: Cocoonbox_Control_V1_RotateKeyRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_Key {
        try await keys.rotate(request.name, request.value)
    }

    func delete(request: Cocoonbox_Control_V1_KeyRef, context: ServerContext) async throws -> Cocoonbox_Control_V1_DeleteKeyResponse {
        try await keys.delete(request.name)
        return .init()
    }
}
