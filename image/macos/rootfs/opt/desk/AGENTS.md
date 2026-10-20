# Desk scripts on this box

This box is a macOS desktop (macOS 26, 1280x800), with the menu bar at the top and the Dock at the
bottom. TextEdit opens `.txt` and `.rtf` files.
Desk scripts drive it from inside: you write a script, the box runs it next to the screen, and
you get its result, its logs and a screenshot back. You plan; the script only executes.

- Runtime: Node 24, which runs TypeScript with its types stripped (no build step).
- API: `import * as desk from '/opt/desk/lib/desk.mts'`. Every call is async: `await` it.
  The source is short; read `/opt/desk/lib/desk.mts` for the exact signatures.
- The same calls work from a shell: `desk VERB key=value ...` prints JSON, for example
  `desk snapshot window=Untitled text=true` or `desk press id=1.27/379`.

<!-- routing -->

## The desk API

Windows and elements come from the macOS accessibility tree (AX), roles named as in ARIA.

| Call | Does |
|---|---|
| `windows()` | top-level windows `{id, title, role, pid, active}`; `focused()` the active one; a modal dialog the screen does not show also has `modal` (`hidden`, `off the screen` or `behind "TITLE"`) and `blocks`, the title of the window it blocks |
| `wait(since, timeout_ms)` | waits until the window list differs from `since` (a `windows()` result), at most `timeout_ms`: `{changed, windows}` |
| `snapshot(window?, {pid?, keys?})` | the elements of one window (default: the focused one) in tree order: `{ref, id, parent, depth, role, name, states, value?, bounds?}`; `states` holds `checked`, `disabled`, `focused`, `showing`, `selected`, `expanded` … |
| `outline(window?)` | the same tree as text, one `- role "name" [checked] [id=ID]: "value"` line per element |
| `find(name, {role?, window?})` | the elements named `name` (else containing it), ignoring case and punctuation |
| `read(id, {text?})` | one element read again; `text: true` returns its whole text as `value` |
| `press(id)` | presses an element as a user would: selects a tab or list row, opens the menus above a hidden menu entry, clicks a shown control, else runs its accessibility action |
| `act(id, 'click' \| 'action' \| 'select' \| 'focus')` | one of those ways only |
| `at(x, y, window?)` | the element at a screen point and the elements above it |
| `key('cmd+shift+s')`, `typeText('text')`, `click(x, y, {button?, count?})` | desktop input to the focused window; `typeText` turns each newline into Return and returns once the focused field holds the text (at most 0.1 s more) |
| `focus(window)`, `close(window)`, `launch(app)` | bring a window to the front, ask it to close as its close button does, start an app |
| `screen()`, `shot({x?, y?, w?, h?})` | the screen size; a PNG `Buffer` of a screen rectangle |
| `decide(state, {key: yes(q) \| choice(q, options)})` | a fast judgment over text or JSON state (about 0.3 s): `yes` answers `{probability}`, `choice` answers `{choice, confidence, probabilities}` |
| `ask(prompt, png \| [png])` | a vision model's answer about screenshots |
| `ground(png, target, {left?, top?, width, height})` | the screen point of a described element in a screenshot of that area |

Element ids stay valid while the element exists; take a new snapshot after a window changes.

Shapes that scripts get wrong:

- `snapshot()` returns `{window, nodes, truncated, ms}`: loop over `s.nodes`. A node has `bounds`
  only when you ask for them: `snapshot(title, {bounds: true})`.
- `press`, `read` and `act` take a node's `id` (such as `1.17/62`, `[id=1.17/62]` in an outline).
  A node's `ref` is only its position in that one snapshot, and no call takes it. To act on a named
  element: `const {found} = await desk.find('Save', {role: 'button'}); await desk.press(found[0].id)`.
- "UnknownObject" or "bad element id": the element is gone (a list refreshed, a menu closed, a
  folder changed). `find` it again.
- A window title is only the document or folder name (`Untitled`, `Documents`), and a window's
  `app` names its app. A call that takes a window title looks it up each time; look it up again
  after the title changes: `(await desk.windows()).find((w) => w.app === 'TextEdit')`.

## GUI plans and saved flows

`gui_plan` runs `/opt/desk/lib/gui_plan.mts` on this API: a list of steps (app, menu, click,
check, set, type, key, wait, verify, close), each resolved on the accessibility tree and checked,
stopping at the first surprise. A passing plan that starts with an app step is saved as
`/workspace/flows/<name>.json` (a git repository, with the element each step hit) and listed in
`/workspace/flows/INDEX`; `gui_plan {flow, params}` replays it without a model call while the
saved elements still match. `gui_plan {save: name}` alone saves the plan that just ran, or the
steps that passed before an escalation, without running them again. Every run's steps and timings
go to `/workspace/flows/.runs.jsonl`. Saved flows and scripts in `/workspace/flows` follow the user
to each of their boxes of this kind, and `cocoonbox flows` exports and imports them on the Mac.
`gui_plan {flows: true}` lists the saved flows. When a saved flow escalates, handle the reason,
then continue with `gui_plan` from the failing step while that screen is still up, so the fixed
run is saved.

## Run a script

Call `script_run` with `code` (an ES module) or `script` (a file in the box, relative to
`/workspace`), and `params`. When the module exports `main(params)`, it is called with them and
its return value comes back as `result`. `timeout_s` defaults to 90, at most 110; the run is
stopped after it. The reply is JSON `{result, logs, error?: {kind: exception|timeout|interrupted,
message}, duration_ms, exit_code, stderr, snapshot?, screenshot_path}` and a screenshot taken after
the run. `logs` is the script's stdout. Set `globalThis.$snapshot` to a string, such as an outline,
to get it back as `snapshot`. Nothing persists between runs.

From a shell in the box: `node /workspace/flows/x.mts`; `main` is not called for you there.

```ts
// /workspace/flows/save-note.mts
import * as desk from '/opt/desk/lib/desk.mts'

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))
// poll a condition every 50 ms, fail after `ms`
async function until(what: string, ok: () => Promise<boolean>, ms = 5000) {
  for (const end = Date.now() + ms; !(await ok()); await sleep(50)) if (Date.now() > end) throw new Error(`no ${what} within ${ms} ms`)
}
const front = async () => (await desk.focused())?.title ?? ''

export async function main(p: { name?: string; text?: string } = {}) {
  const { name = 'note', text = 'hello' } = p
  await desk.launch('TextEdit')
  await until('a TextEdit window', async () => (await desk.windows()).some((w) => w.app === 'TextEdit'))
  await desk.key('cmd+n')
  await desk.typeText(text)
  await desk.key('cmd+s')
  await until('the save sheet', async () => (await desk.find('Save As:', {})).found.length > 0)
  await desk.key('cmd+a')
  await desk.typeText(name)
  await desk.key('return')
  await until('saved document', async () => (await front()).startsWith(name))
  return { saved: await front() }
}
```

Save a script that works as `/workspace/flows/<name>.mts` and replay it with new params:
`script_run {script: "flows/save-note.mts", params: {name: "b", text: "bye"}}`.

## Write scripts that hold up

- Find elements on the accessibility tree first; ground with a screenshot only what has no name.
- After an action that changes state, look again (`read`, a snapshot, `decide` or `ask`) instead
  of assuming it worked.
- Wait for the condition the next step needs (a window title, an element state) by polling it;
  a fixed sleep is either too short or wasted time.
- Open dialogs by their keyboard shortcut (`cmd+s`, `cmd+shift+s`): more reliable than toolbar clicks.
- Get one step working, then join the steps into a flow.

## Finish the work

- Do what the user, a reviewer or the brief asks; name a doubt in your answer instead of refusing.
- A number you add takes the format of the numbers next to it (decimals, separators, units).
- Before the final answer, check the whole output against each point of the request, not only the
  part you changed last.
- `shell` stops a command after 120 s by default and resets its session: pass `timeout_s` (at most
  600) for a longer install, build or conversion.

## Traps on this desktop

- Shortcuts use `cmd`: `cmd+s` saves, `cmd+w` closes a window, `cmd+q` quits an app, `cmd+shift+g`
  opens Go to Folder in Finder and in open and save dialogs.
- An app keeps running after its last window closes; `cmd+q` quits it.
- Closing an unsaved new document opens a sheet with Delete, Cancel and Save; press Delete to
  discard it.
- Open and save dialogs are sheets of their document window: find their fields by label (`Save As:`).
  Type a folder path into Go to Folder (`cmd+shift+g`), then Return.
- Finder: `cmd+shift+g` and a path open a folder; `cmd+shift+n` makes a folder; Return renames the
  selected item.
- A menu toggle's check mark reads true only after its menu has opened once; `gui_plan` opens the
  menu before it reads a toggle.
- Text substitution (automatic capitalization, smart quotes and dashes) is off on this box, so
  typed text stays as typed.
- A snapshot of a Chrome window holds the browser's own controls and the start of the page;
  page content goes to the `browser_*` tools.
- Apps a script launches keep running after it ends, also after a timeout.
- A document with layout (slides, a spreadsheet, a formatted text) keeps it only when edited in its
  app; a library that rebuilds the file from code drops what it does not model.
- The box's user can see the screen: keep windows tidy and close the dialogs you open.
