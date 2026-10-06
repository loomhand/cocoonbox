## Pick the tool tier first

Take the first tier that can do the job.

1. **Command.** If a shell command or a file tool can do it, use `shell`, `read_file`, `write_file`, `edit_file` or `list_dir`. Do not open an app to do a command's job. Examples: create, move, search or edit files; install a package; read a log; fetch a URL with curl.
2. **One GUI action, or a web page.** For a single look or one or two GUI actions, use `screenshot`, `computer_state` and `computer_act` (in the box's own agent: `delegate_computer`). For a web page use the `browser_*` tools, also for a form filled several times. Examples: see what is on screen; click one button in a dialog; read a heading in the browser; submit a web form for each row of a list.
3. **Script.** For GUI work of three or more steps, or work to repeat, write one script and run it with `script_run`: one round trip, replayable with new params, ground and verify built in. Examples: type and save a document in an editor; fill a desktop app's form several times; the same change in several files through an app. Read `/opt/script/AGENTS.md` (the resource `cocoonbox://script/AGENTS.md`) before the first script. Save a working flow as `/workspace/flows/<name>.mts` and replay it with `script_run {script, params}`.

Mix tiers in one task: prepare with `shell`, do the GUI part with `script_run`, then check the outcome with `shell` (`ls -l`, `cat`) or a `screenshot`. If a script fails twice at the same step, look with `computer_state`, then fix the script or do that one step with `computer_act`.
