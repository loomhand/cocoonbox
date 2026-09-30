import Foundation

// Plugin is the cocoonbox plugin as a local marketplace (design.md §13.5):
// manifests for Claude Code and Codex (Cursor reads the Claude one), one
// .mcp.json that runs the bridge, a skill and slash commands. No file holds
// a URL or a token; `command` is the path of the cocoonbox CLI.
public enum Plugin {
    public static let name = "cocoonbox"
    public static let marketplace = "cocoonbox"
    static let version = "0.1.0"
    static let summary = "Drive your cocoonbox cloud desktop: its shell, files, browser and screen, and its lifecycle (wake, save, upgrade, hibernate)."

    // files maps paths under the marketplace root to their contents.
    public static func files(command: String) -> [String: String] {
        [
            ".claude-plugin/marketplace.json": json([
                "name": marketplace,
                "owner": ["name": "CMGS"],
                "metadata": ["description": summary],
                "plugins": [["name": name, "source": "./\(name)", "description": summary]],
            ]),
            ".agents/plugins/marketplace.json": json([
                "name": marketplace,
                "interface": ["displayName": "cocoonbox"],
                "plugins": [[
                    "name": name,
                    "source": ["source": "local", "path": "./\(name)"],
                    "policy": ["installation": "AVAILABLE", "authentication": "ON_INSTALL"],
                    "category": "Developer Tools",
                ]],
            ]),
            "\(name)/.claude-plugin/plugin.json": json(["name": name, "version": version, "description": summary, "author": ["name": "CMGS"]]),
            "\(name)/.codex-plugin/plugin.json": json([
                "name": name, "version": version, "description": summary,
                "mcpServers": "./.mcp.json", "skills": "./skills/",
                "interface": ["displayName": "cocoonbox", "shortDescription": summary, "category": "Developer Tools"],
            ]),
            "\(name)/.mcp.json": mcpConfig(command: command),
            "\(name)/skills/cocoonbox/SKILL.md": skill,
            "\(name)/commands/status.md": slash("Show the state of my cocoonbox", "Call the cocoonbox tool box_status and tell me the state, the image, whether an upgrade is available and when my files were last saved."),
            "\(name)/commands/wake.md": slash("Wake my cocoonbox", "Call the cocoonbox tool box_ensure and tell me when the box is running."),
            "\(name)/commands/save.md": slash("Save my cocoonbox now", "Call the cocoonbox tool box_save. Tell me it closes the box's browser during the save, and report the result."),
            "\(name)/commands/sleep.md": slash("Hibernate my cocoonbox", "Call the cocoonbox tool box_hibernate and tell me when the box is hibernated."),
        ]
    }

    // mcpConfig is the stdio server entry every client runs.
    public static func mcpConfig(command: String) -> String {
        json(["mcpServers": [name: ["command": command, "args": ["mcp"]]]])
    }

    public static func write(to root: URL, command: String) throws {
        let manager = FileManager.default
        for (path, content) in files(command: command) {
            let file = root.appendingPathComponent(path)
            try manager.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
            try Data(content.utf8).write(to: file, options: .atomic)
        }
    }

    static let skill = """
    ---
    name: cocoonbox
    description: Work on the user's cocoonbox cloud desktop (a Linux desktop in the cloud, not this Mac) through the cocoonbox MCP tools. Use when the user asks for something on their box, their cloud desktop or cocoonbox.
    ---

    The cocoonbox tools act on the user's cocoonbox cloud desktop, not on this Mac. Local files are not the target.

    - Check `box_status` first when the state matters. A box tool wakes a hibernated box by itself; `box_ensure` wakes it on purpose.
    - Use `shell`, `read_file`, `write_file`, `edit_file` and `list_dir` for files and commands on the box, the `browser_*` tools for its browser, and `computer_state`, `computer_act` and `screenshot` for its desktop.
    - After a GUI action, take a `screenshot` to confirm it worked.
    - Call `box_save` before the user leaves for a long time. It closes the box's browser during the save.
    - `box_upgrade` restarts running programs and keeps files, logins and agent history; ask the user first, then call it with `confirm: true`.
    - Call `box_hibernate` when the user is done.

    """

    private static func slash(_ description: String, _ body: String) -> String {
        "---\ndescription: \(description)\n---\n\n\(body)\n"
    }

    private static func json(_ object: [String: Any]) -> String {
        let data = (try? JSONSerialization.data(withJSONObject: object, options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes])) ?? Data()
        return String(decoding: data, as: UTF8.self) + "\n"
    }
}
