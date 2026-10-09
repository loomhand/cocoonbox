## Pick the tool tier first

Rule: GUI work in a desktop app (menus, toggles, dialogs, Preferences, Save As, settings windows, typing and saving a document) goes to `gui_plan`: the whole plan as steps in one call, without looking first; it checks each step and escalates a surprise. A saved flow that fits replays with `gui_plan {flow, params}`; `gui_plan {save: name}` alone saves the last plan without running it again. A loop over many items, data between apps, or logic a plan cannot hold goes to one script run with `script_run`; read `/opt/script/AGENTS.md` before the first script, save a working one as `/workspace/flows/<name>.mts` and replay it with `script_run {script, params}`.

Otherwise take the first tier that fits, for each request:
1. Command: `shell` and the file tools. Do not open an app to do a command's job.
2. One look, or a click no plan can name: `screenshot`, `computer_state`, `computer_act` (in the box's own agent: `delegate_computer`). A web page, also a form filled several times: the `browser_*` tools.

If a plan or script fails twice at one step, look with `computer_state`, then fix it or do that step with `computer_act`.
