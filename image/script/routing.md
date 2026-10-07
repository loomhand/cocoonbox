## Pick the tool tier first

Decide for each request, not by habit from the previous one. Take the first tier that fits.

1. **Command.** If a shell command or a file tool can do it, use `shell`, `read_file`, `write_file`, `edit_file` or `list_dir`. Do not open an app to do a command's job. Examples: count, create, move, search or edit files; install a package; read a log; fetch a URL with curl.
2. **A look, one click, or a web page.** For a single look or one click, use `screenshot`, `computer_state` and `computer_act` (in the box's own agent: `delegate_computer`). For a web page use the `browser_*` tools, also for a form filled several times. Examples: see what is on screen; toggle one menu option; read a page's title; submit a web form for each row of a list.
3. **Script.** Other desktop-app work is complex, even when it looks short: write one script and run it with `script_run` (in a client that loads tools on demand, load `script_run` first). Complex means, for example:
   - typing and saving documents in a desktop app;
   - any Save As, Open or Preferences dialog;
   - filling a desktop app's form, once or repeatedly;
   - the same edit across several files or items through an app;
   - renaming or moving several items in a file manager;
   - copying data from one app into another;
   - waiting for a window or dialog, or checking a state before going on;
   - finding a control by its accessible name, or with ground and verify, instead of by pixels;
   - anything the user may want to replay later with new params.

   Read `/opt/script/AGENTS.md` (the resource `cocoonbox://script/AGENTS.md`) before the first script. Save a working flow as `/workspace/flows/<name>.mts` and replay it with `script_run {script, params}`.

Mix tiers in one task: prepare with `shell`, do the GUI part with `script_run`, then check the outcome with `shell` (`ls -l`, `cat`) or a `screenshot`. If a script fails twice at the same step, look with `computer_state`, then fix the script or do that one step with `computer_act`.
