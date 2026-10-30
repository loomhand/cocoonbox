## Pick the tool tier first

GUI work in a desktop app (menus, toggles, dialogs, settings, Save As, typing a document) goes to `gui_plan`: the whole plan in one call, without looking first. Loops over many items, data between apps, or logic a plan cannot hold: one `script_run`.

Else take the first tier that fits:
1. Command: `shell` and the file tools; `browser_fetch` to read a web page. Do not open an app to do a command's job, nor rebuild documents in code.
2. One look, or a click no plan can name: `screenshot`, `computer_state`, `computer_act`. A web page to click, type or sign in on, or a form filled often: the `browser_*` tools.

A plan or script that fails twice at one step: look with `computer_state`, then fix it or use `computer_act`. Watch a chat or inbox by polling its activity list, not just known pages.

Before finishing, check the whole result in its app or a render. If the task says an input changes later, wait and re-check. Copy numbers at the source's precision. Save deliverables at the path the user names, else in their home, not /workspace. Edit a file an app has open in that app, or close the app first. If unclear, ask all questions at once. Explicit user constraints beat your optimising.
