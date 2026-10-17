import CocoonBoxKit
import Foundation
import GRPCCore

// MockAccount is the user's sign-ins as the api keeps them: the CLI's device is the caller's, and a revoke takes a device away.
actor MockAccount {
    private var devices: [Cocoonbox_Control_V1_Device] = {
        let now = Int64(Date().timeIntervalSince1970 * 1000)
        return [
            .with {
                $0.id = "dev_app0000000000000"
                $0.client = "cocoonbox-app"
                $0.label = "studio"
                $0.createdAtMs = now - 60000
                $0.usedAtMs = now
            },
            .with {
                $0.id = "dev_cli0000000000000"
                $0.client = "cocoonbox-cli"
                $0.label = "laptop"
                $0.createdAtMs = now - 120_000
                $0.current = true
            },
        ]
    }()

    func list() -> [Cocoonbox_Control_V1_Device] {
        devices
    }

    func revoke(_ id: String) throws {
        guard devices.contains(where: { $0.id == id }) else { throw RPCError(code: .notFound, message: "device not found") }
        devices.removeAll { $0.id == id }
    }
}

struct AccountService: Cocoonbox_Control_V1_Account.SimpleServiceProtocol {
    let account: MockAccount

    func listDevices(request: Cocoonbox_Control_V1_ListDevicesRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_ListDevicesResponse {
        let all = await account.list()
        return .with { $0.devices = all }
    }

    func revokeDevice(request: Cocoonbox_Control_V1_RevokeDeviceRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_RevokeDeviceResponse {
        try await account.revoke(request.deviceID)
        return .init()
    }

    func revokeToken(request: Cocoonbox_Control_V1_RevokeTokenRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_RevokeTokenResponse {
        .init()
    }
}
