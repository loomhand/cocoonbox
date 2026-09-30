import Foundation

// ToolCatalog holds the tools the bridge lists without asking the box: the
// control tools from contracts.md "Control MCP", and the box tools from the
// cache of the box's last answer, else the contract's names with open schemas.
public enum ToolCatalog {
    nonisolated(unsafe) public static let control: [[String: Any]] = [
        tool("box_status", "Show the state of your cocoonbox cloud desktop: running or hibernated, its image, whether an upgrade is available, and when your files were last saved.", [:]),
        tool("box_ensure", "Start or wake your cocoonbox cloud desktop and wait until it is running. Waking takes a few seconds; creating a new box takes longer.", ["wait": ["type": "boolean", "default": true]]),
        tool("box_hibernate", "Hibernate your cocoonbox cloud desktop. Running programs and memory are kept and come back on the next wake.", [:]),
        tool("box_save", "Save your files, logins and agent history from the box to storage now. The browser in the box closes during the save.", [:]),
        tool("box_upgrade", "Move your box to the latest desktop image. Running programs restart; files, logins and agent history are kept. Requires confirm: true.", ["confirm": ["type": "boolean"]], required: ["confirm"]),
    ]

    public static let controlNames = Set(control.compactMap { $0["name"] as? String })

    // boxNames are the per-box tools of contracts.md "Remote tools over MCP".
    public static let boxNames = [
        "shell", "read_file", "write_file", "edit_file", "list_dir", "browser_navigate", "browser_snapshot",
        "browser_click", "browser_type", "browser_read", "computer_state", "computer_act", "screenshot",
    ]

    public static var boxFallback: [[String: Any]] {
        boxNames.map { ["name": $0, "description": "A tool on your cocoonbox cloud desktop. Its full schema loads once the box has run.", "inputSchema": ["type": "object"]] }
    }

    public static func loadBox(from file: URL) -> [[String: Any]]? {
        guard let data = try? Data(contentsOf: file), let tools = try? JSONSerialization.jsonObject(with: data) as? [[String: Any]], !tools.isEmpty else { return nil }
        return tools
    }

    public static func saveBox(_ tools: [[String: Any]], to file: URL) {
        try? FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
        if let data = try? JSONSerialization.data(withJSONObject: tools, options: [.sortedKeys]) {
            try? data.write(to: file, options: .atomic)
        }
    }

    static func same(_ a: [[String: Any]], _ b: [[String: Any]]) -> Bool {
        let encode = { (tools: [[String: Any]]) in try? JSONSerialization.data(withJSONObject: tools, options: [.sortedKeys]) }
        return encode(a) == encode(b)
    }

    private static func tool(_ name: String, _ description: String, _ properties: [String: Any], required: [String] = []) -> [String: Any] {
        ["name": name, "description": description, "inputSchema": ["type": "object", "properties": properties, "required": required]]
    }
}
