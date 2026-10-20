# Desk scripts on this box

This box is a Windows 11 desktop (1280x800), with the taskbar at the bottom. Notepad opens `.txt`
files, and File Explorer browses folders.
Desk scripts drive it from inside: you write a script, the box runs it next to the screen, and
you get its result, its logs and a screenshot back. You plan; the script only executes.

- Runtime: Node 24, which runs TypeScript with its types stripped (no build step).
- API: `import * as desk from 'file:///C:/ProgramData/cocoonbox/desk/lib/desk.mts'`. Node takes
  a Windows path only as a `file:///` URL. Every call is async: `await` it. The source is short;
  read `C:\ProgramData\cocoonbox\desk\lib\desk.mts` for the exact signatures.
- `shell` runs Git Bash: `/c/workspace` is `C:\workspace`, and Windows programs take `C:/...` paths.

<!-- routing -->

## The desk API

Windows and elements come from Windows UI Automation (UIA), roles named as in ARIA.

| Call | Does |
|---|---|
| `windows()` | top-level windows `{id, title, role, pid, active, app}`, `app` being the executable; `focused()` the active one |
| `wait(since, timeout_ms)` | waits until the window list differs from `since` (a `windows()` result), at most `timeout_ms`: `{changed, windows}` |
| `snapshot(window?, {pid?, keys?})` | the elements of one window (default: the focused one) in tree order: `{ref, id, parent, depth, role, name, states, value?, bounds?}`; `states` holds `checked`, `disabled`, `focused`, `showing`, `selected`, `expanded` … |
| `outline(window?)` | the same tree as text, one `- role "name" [checked] [id=ID]: "value"` line per element |
| `find(name, {role?, window?})` | the elements named `name` (else containing it), ignoring case and punctuation |
| `read(id, {text?})` | one element read again; `text: true` returns its whole text as `value` |
| `press(id)` | presses an element as a user would: selects a tab or list row, opens the menus above a hidden menu entry, clicks a shown control, else runs its UIA pattern |
| `act(id, 'click' \| 'action' \| 'select' \| 'focus')` | one of those ways only |
| `at(x, y, window?)` | the element at a screen point and the elements above it |
| `key('ctrl+shift+s')`, `typeText('text')`, `click(x, y, {button?, count?})` | desktop input to the focused window; `typeText` turns each newline into Enter and returns once the focused field holds the text (at most 0.1 s more) |
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
- A window title carries the document or folder name and the app (`notes.txt - Notepad`,
  `Documents - File Explorer`); look a window up again after its title changes:
  `(await desk.windows()).find((w) => w.title.endsWith('- Notepad'))`.

## GUI plans and saved flows

`gui_plan` runs the shuttle runtime's `gui_plan.mts` on this API: a list of steps (app, menu,
click, check, set, type, key, wait, verify, close), each resolved on the UIA tree and checked,
stopping at the first surprise. A passing plan that starts with an app step is saved as
`C:\workspace\flows\<name>.json` (a git repository, with the element each step hit) and listed in
`C:\workspace\flows\INDEX`; `gui_plan {flow, params}` replays it without a model call while the
saved elements still match. `gui_plan {save: name}` alone saves the plan that just ran, or the
steps that passed before an escalation, without running them again. Every run's steps and timings
go to `C:\workspace\flows\.runs.jsonl`. `gui_plan {flows: true}` lists the saved flows. When a
saved flow escalates, handle the reason, then continue with `gui_plan` from the failing step while
that screen is still up, so the fixed run is saved.

## Run a script

Call `script_run` with `code` (an ES module) or `script` (a file in the box, relative to
`C:\workspace`), and `params`. When the module exports `main(params)`, it is called with them and
its return value comes back as `result`. `timeout_s` defaults to 90, at most 110; the run is
stopped after it. The reply is JSON `{result, logs, error?: {kind: exception|timeout|interrupted,
message}, duration_ms, exit_code, stderr, snapshot?, screenshot_path}` and a screenshot taken after
the run. `logs` is the script's stdout. Set `globalThis.$snapshot` to a string, such as an outline,
to get it back as `snapshot`. Nothing persists between runs.

```ts
// C:\workspace\flows\save-note.mts
import * as desk from 'file:///C:/ProgramData/cocoonbox/desk/lib/desk.mts'

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))
// poll a condition every 50 ms, fail after `ms`
async function until(what: string, ok: () => Promise<boolean>, ms = 5000) {
  for (const end = Date.now() + ms; !(await ok()); await sleep(50)) if (Date.now() > end) throw new Error(`no ${what} within ${ms} ms`)
}
const front = async () => (await desk.focused())?.title ?? ''

export async function main(p: { name?: string; text?: string } = {}) {
  const { name = 'note', text = 'hello' } = p
  await desk.launch('notepad')
  await until('a Notepad window', async () => (await front()).endsWith('- Notepad'))
  await desk.typeText(text)
  await desk.key('ctrl+s')
  await until('the save dialog', async () => (await desk.find('File name:', {})).found.length > 0)
  await desk.typeText(`C:\\workspace\\${name}.txt\n`)
  await until('saved document', async () => (await front()).startsWith(name))
  return { saved: await front() }
}
```

Save a script that works as `C:\workspace\flows\<name>.mts` and replay it with new params:
`script_run {script: "flows/save-note.mts", params: {name: "b", text: "bye"}}`.

## Write scripts that hold up

- Find elements on the UIA tree first; ground with a screenshot only what has no name.
- After an action that changes state, look again (`read`, a snapshot, `decide` or `ask`) instead
  of assuming it worked.
- Wait for the condition the next step needs (a window title, an element state) by polling it;
  a fixed sleep is either too short or wasted time.
- Open dialogs by their keyboard shortcut (`ctrl+s`, `ctrl+shift+s`): more reliable than toolbar clicks.
- Get one step working, then join the steps into a flow.

## Finish the work

- Do what the user, a reviewer or the brief asks; name a doubt in your answer instead of refusing.
- A number you add takes the format of the numbers next to it (decimals, separators, units).
- Before the final answer, check the whole output against each point of the request, not only the
  part you changed last.
- `shell` stops a command after 120 s by default and resets its session: pass `timeout_s` (at most
  600) for a longer install, build or conversion.

## Traps on this desktop

- Shortcuts use `ctrl`: `ctrl+s` saves, `ctrl+w` closes a tab or window, `alt+f4` closes an app's
  window.
- The save and open dialogs take a whole path in their File name field: type `C:\path\name.txt`
  and Enter.
- File Explorer's navigation pane needs a real click to navigate: `press` on its tree selects an
  item without opening it, so `click` its point or type the path into the address bar (`alt+d`).
- Closing an unsaved document asks whether to save it; press Don't save to discard it.
- A snapshot of a Chrome window holds the browser's own controls and the start of the page;
  page content goes to the `browser_*` tools.
- Apps a script launches keep running after it ends, also after a timeout.
- A document with layout (slides, a spreadsheet, a formatted text) keeps it only when edited in its
  app; a library that rebuilds the file from code drops what it does not model.
- The box's user can see the screen: keep windows tidy and close the dialogs you open.
