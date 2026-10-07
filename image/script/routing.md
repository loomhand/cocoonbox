## Pick the tool tier first

Rule: desktop-app work with typing, a dialog, several items or more than a couple of clicks goes to one script run with `script_run`. That covers typing and saving documents, any Save As, Open or Preferences dialog, a desktop app's form, the same edit on several items, file-manager batches, copying between apps, waiting for a window, and anything to replay later. Read `/opt/script/AGENTS.md` before the first script; save a working flow as `/workspace/flows/<name>.mts` and replay it with `script_run {script, params}`.

Otherwise take the first tier that fits, for each request:
1. Command: `shell` and the file tools. Do not open an app to do a command's job.
2. One look or a couple of clicks, such as toggling one menu option: `screenshot`, `computer_state`, `computer_act` (in the box's own agent: `delegate_computer`). A web page, also a form filled several times: the `browser_*` tools.

If a script fails twice at one step, look with `computer_state`, then fix the script or do that step with `computer_act`. For a borderline task, `cua_route` gives a quick second opinion.
