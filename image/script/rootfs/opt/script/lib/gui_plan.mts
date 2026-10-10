// gui_plan.mts: run a GUI plan in the box without a model turn per step, save a passing plan as a flow, replay flows.
// params: {steps, params?, save?, start?} | {save} | {flow, params?, start?} | {find} | {reindex}; result: {ok, done, ms, trace, saved?, escalate?}
import { execFileSync, spawn } from 'node:child_process'
import { appendFileSync, existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from 'node:fs'
import { createConnection } from 'node:net'
import {
  AccessibilitySnapshot, AskModel, Button, choice, Coordinate, DecisionModel, Direction, FocusPolicy, GroundingModel, Image, Key, Machine, noul,
  Visibility,
} from '@vendor/script-js'

type Step = {
  app?: string; title?: string; menu?: string; click?: string; check?: string; on?: boolean; set?: string; value?: string
  type?: string; key?: string; wait?: string; expect?: string; verify?: string; close?: boolean; role?: string; window?: string
  timeout_ms?: number; enabled?: boolean; fp?: Fp; seen?: string; blank?: Fp
}
type Fp = { role: string; name: string; parent: string; nth: number; label?: string }
type Node = { ref: number; depth: number; role: string; name: string; value: string; disabled: boolean; parent: number; keys: string; accel: string }
type Hit = { n: Node; i: number; how: string; p?: number; margin?: number }
type Flow = { name: string; params: Record<string, string>; steps: Step[]; idempotent: boolean; runs: number; saved_at: string; rewrites: number }
type Chain = { at: number; window: string | null; pid: number | null; name?: string; named?: boolean; params: Record<string, string>; steps: Step[]; dirty: boolean; saved?: boolean; escalated?: boolean }

const FLOWS = '/workspace/flows'
const LOG = `${FLOWS}/.runs.jsonl`
const INDEX = `${FLOWS}/INDEX`
const PENDING = `${FLOWS}/.pending.json`
const CHAIN_AGE = 10 * 60 * 1000
const ACTIVE = new Set(['button', 'checkbox', 'menuitem', 'menu', 'tab', 'textbox', 'spinbutton', 'slider', 'combobox', 'radio', 'link', 'switch', 'option', 'treeitem', 'togglebutton', 'searchbox', 'menuitemcheckbox', 'menuitemradio'])
const TOGGLES = 'checkbox|switch|togglebutton|menuitemcheckbox'
const FIELDS = 'textbox|searchbox|spinbutton|slider|combobox'
const LINE = /^(\s*)- (\S+)(?: "((?:[^"\\]|\\.)*)")?(.*?)\[ref=(\d+)\](?:: "(.*)")?$/
const MARGIN = 0.25
const HOLDERS = /^at-spi\.role\.(page|filler|panel|scroll|viewport|layered|frame|dialog|window)\b/
const OPS = ['app', 'menu', 'click', 'check', 'set', 'type', 'key', 'wait', 'expect', 'verify', 'close']
const m = Machine.local()
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))
const norm = (s: string) => s.toLowerCase().replace(/[_…]|\.\.\.|:$/g, '').replace(/\s+/g, ' ').trim()
const slug = (s: string) => norm(s).replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '').slice(0, 40)
const session = `gp-${process.pid}`
let jev: DecisionModel | null = null
let busNames: string[] | null = null
const known = new Set<string>()
let lastSeen = ''
let recording = false
let found = false
let typed = false
let T: Record<string, number> = {}
let screen: { left: number; top: number; right: number; bottom: number } | null = null
const tick = (k: string, t0: number) => { T[k] = (T[k] ?? 0) + Date.now() - t0 }

class Surprise extends Error {
  reason: string
  seen: Record<string, unknown>
  constructor(reason: string, seen: Record<string, unknown> = {}) { super(reason); this.reason = reason; this.seen = seen }
}

function parse(snap: AccessibilitySnapshot): Node[] {
  const out: Node[] = []; const stack: number[] = []
  for (const l of snap.print({ maxChars: 80 }).split('\n')) {
    const g = LINE.exec(l); if (!g) continue
    const depth = g[1].length / 2; stack.length = depth
    out.push({ ref: +g[5], depth, role: g[2], name: (g[3] ?? '').replace(/\\"/g, '"').trim(), value: g[6] ?? '', disabled: g[4].includes('[disabled]'), parent: depth ? stack[depth - 1] : -1, keys: /\[(<[^\]]*)\]/.exec(g[4])?.[1] ?? '', accel: /\[[^\]<;]*;[^\];]*;([^\];]*)\]/.exec(g[4])?.[1] ?? '' })
    stack[depth] = out.length - 1
  }
  return out
}

const focused = () => m.focusedWindow()
// a window that closed after it was listed answers X11 BadWindow to a pid read; its pid is then null
function pidOf(w: { pid: number }): number | null {
  try { return w.pid } catch (e) { if (/error_kind: Window\b/.test(String(e))) return null; throw e }
}
// a window AT-SPI cannot map (an untitled alert) falls back to the newest same-titled child of the focused app
function snapOf(w: NonNullable<ReturnType<typeof focused>>) {
  try { return AccessibilitySnapshot.fromWindow(w) } catch (e) {
    const kids = m.focusedRoot().children(); const titles = new Set(m.windows().map((x) => x.title.trim()))
    const k = kids.filter((k) => k.name.trim() === w.title.trim()).at(-1) ?? kids.filter((k) => !titles.has(k.name.trim())).at(-1)
    if (!k) throw e
    return AccessibilitySnapshot.fromNode(k)
  }
}
function outline(w = focused()) {
  if (!w) return '(no focused window)'
  try { const s = snapOf(w); return s.print({ interactiveOnly: s.len() > 60, maxChars: 100 }).slice(0, 1800) } catch (e) { return String(e) }
}
function seen() {
  const w = focused()
  return { window: w?.title ?? null, windows: m.windows().map((x) => x.title).filter((t) => !/^(Desktop|xfce4-panel)$/.test(t)), outline: outline(w) }
}
const blocked = (o: string) => /AT-SPI peer \S+ is not responding/.test(o)
const unanswered = (title: string) => `"${title}" answers no accessibility calls (a dialog opened by an accessibility action blocks its app, and a key would deadlock it): close it with {close: true}, then reopen it by its keyboard shortcut`
const isAlert = (o: string) => /^- alert/.test(o)
const isDialog = (o: string) => /^- (alert|dialog|file chooser)/.test(o)
// a dialog AT-SPI maps before its children answers with its root alone; read it again until the children show
async function filled(w: NonNullable<ReturnType<typeof focused>>) {
  let o = outline(w)
  for (let i = 0; i < 20 && isDialog(o) && !o.trim().includes('\n'); i++) { await sleep(50); o = outline(w) }
  return o
}

function decide(state: unknown, q: Record<string, any>) {
  const t0 = Date.now(); jev ??= DecisionModel.byAlias('gw_jev')
  try { return jev.decide(state as any, q) } finally { tick('jev', t0); T.jev_calls = (T.jev_calls ?? 0) + 1 }
}

function vision(prompt: string, shot: { base64(): string }) {
  const t0 = Date.now()
  try {
    const a = AskModel.byAlias('gw_verify').ask(`${prompt}\nAnswer with one JSON object and nothing else.`, null, [Image.fromBase64(shot.base64())])
    return JSON.parse(a.slice(a.indexOf('{'), a.lastIndexOf('}') + 1))
  } finally { tick('vision', t0) }
}

// cua-driver get_window_state over its socket: the checked state AT-SPI publishes for check boxes and toggles
function driver(name: string, args: Record<string, unknown>): Promise<any> {
  return new Promise((resolve, reject) => {
    const s = createConnection(`${process.env.HOME}/.cache/cua-driver/cua-driver.sock`); let buf = ''
    const timer = setTimeout(() => { s.destroy(); reject(new Error('cua-driver timed out')) }, 3000)
    s.on('connect', () => s.write(JSON.stringify({ method: 'call', name, args: { ...args, session } }) + '\n'))
    s.on('data', (d) => { buf += d; const i = buf.indexOf('\n'); if (i >= 0) { clearTimeout(timer); s.destroy(); resolve(JSON.parse(buf.slice(0, i))) } })
    s.on('error', (e) => { clearTimeout(timer); reject(e) })
  })
}
async function checked(pid: number, name: string): Promise<boolean | null> {
  const t0 = Date.now()
  try {
    const r = await driver('get_window_state', { pid, include_screenshot: false, query: name })
    const els = (r.result?.structuredContent?.elements ?? []).filter((e: any) => norm(e.label ?? '') === norm(name) && /check|toggle|switch|radio/.test(e.role ?? ''))
    return els.length === 1 ? els[0].selected === true : null
  } catch { return null } finally { tick('state', t0) }
}

// a GtkApplication menu toggle: its label maps to an action in org.gtk.Menus, whose state org.gtk.Actions reports
function gdbus(dest: string, path: string, method: string, ...args: string[]) {
  return execFileSync('gdbus', ['call', '--session', '--dest', dest, '--object-path', path, '--method', method, ...args], { encoding: 'utf8', timeout: 2000 })
}
function gtkToggle(pid: number, label: string): boolean | null {
  const t0 = Date.now()
  try {
    const comm = readFileSync(`/proc/${pid}/comm`, 'utf8').trim().toLowerCase()
    busNames ??= [...gdbus('org.freedesktop.DBus', '/org/freedesktop/DBus', 'org.freedesktop.DBus.ListNames').matchAll(/'([\w.-]+)'/g)].map((x) => x[1])
    for (const bus of busNames.filter((n) => n.toLowerCase().split('.').at(-1) === comm)) {
      const root = '/' + bus.replace(/\./g, '/')
      const menus = gdbus(bus, `${root}/menus/menubar`, 'org.gtk.Menus.Start', `[${[...Array(64).keys()].join(',')}]`)
      const actions = [...menus.matchAll(/\{[^{}]*\}/g)].map((x) => x[0]).filter((d) => norm(/'label': <'((?:[^'\\]|\\.)*)'>/.exec(d)?.[1] ?? '') === norm(label))
        .map((d) => /'action': <'([^']+)'>/.exec(d)?.[1]).filter((a): a is string => !!a)
      if (new Set(actions).size !== 1) continue
      const [scope, ...rest] = actions[0].split('.'); const action = rest.join('.')
      const paths = scope === 'app' ? [root] : [...gdbus(bus, `${root}/window`, 'org.freedesktop.DBus.Introspectable.Introspect').matchAll(/<node name="(\d+)"/g)].map((x) => `${root}/window/${x[1]}`)
      const states = new Set(paths.map((p) => /\[<(true|false)>\]/.exec(gdbus(bus, p, 'org.gtk.Actions.Describe', action))?.[1]))
      if (states.size === 1 && !states.has(undefined)) return states.has('true')
    }
    return null
  } catch { return null } finally { tick('state', t0) }
}

// the window the step acts in: the focused one, after waiting for one that matches `want` (title text, else a jev yes/no)
async function windowFor(want: string | undefined, ms = 3000, saw?: string) {
  const t0 = Date.now()
  let w = focused()
  if (!want) {
    // an untitled focused window that is no alert (a completion popup) leaves the step in the window the plan was in
    if (w && !w.title.trim() && lastSeen && !isAlert(outline(w))) w = m.windows().find((x) => x.title === lastSeen && pidOf(x) === pidOf(w!)) ?? w
    tick('wait', t0); if (!w) throw new Surprise('no focused window'); return w
  }
  const end = Date.now() + ms; let asked: string | null = null
  for (;;) {
    w = focused()
    if (w && (norm(w.title).includes(norm(want)) || (saw && w.title === saw))) break
    if (w && known.has(w.title) && Date.now() - t0 > 150) {
      const bg = m.windows().find((x) => norm(x.title).includes(norm(want)))
      if (bg) { bg.focus(); w = bg; break }
    }
    if (w && !known.has(w.title) && w.title !== asked && Date.now() - t0 > 250) {
      asked = w.title
      const o = outline(w)
      if (blocked(o)) { tick('wait', t0); throw new Surprise(unanswered(w.title), seen()) }
      if (isAlert(o)) { tick('wait', t0); throw new Surprise(`alert "${w.title}" opened instead of "${want}"`, seen()) }
      const d = decide({ window_title: w.title, contents: o.slice(0, 800) }, { is: noul(`The plan expects the "${want}" window in front next. Is this window that one, and not a confirmation, error or other unexpected dialog?`) })
      if (d.is.probability >= 0.7) break
      if (d.is.probability < 0.3) { tick('wait', t0); throw new Surprise(`window "${w.title}" opened instead of "${want}"`, seen()) }
    }
    if (Date.now() > end) { tick('wait', t0); throw new Surprise(`expected window "${want}" did not appear`, seen()) }
    await sleep(30)
  }
  known.add(w!.title); tick('wait', t0); return w!
}

const named = (nodes: Node[], i: number) => { for (let p = nodes[i].parent; p >= 0; p = nodes[p].parent) if (nodes[p].name) return nodes[p].name; return '' }
const sibling = (nodes: Node[], i: number) => nodes.slice(Math.max(0, i - 2), i + 3).filter((x) => x.role === 'text' && x.parent === nodes[i].parent && x.name).map((x) => x.name)[0]
function fingerprint(nodes: Node[], h: { n: Node; i: number }): Fp {
  const fp: Fp = { role: h.n.role, name: h.n.name, parent: named(nodes, h.i), nth: 0 }
  const label = sibling(nodes, h.i); if (label) fp.label = label
  fp.nth = matches(nodes, fp).findIndex((x) => x.i === h.i)
  return fp
}
const matches = (nodes: Node[], fp: Fp, within?: number) =>
  candidates(nodes, fp.role, within).filter(({ n, i }) => n.name === fp.name && named(nodes, i) === fp.parent && (sibling(nodes, i) ?? '') === (fp.label ?? ''))
function byFp(nodes: Node[], fp: Fp | undefined, within?: number): Hit | null {
  if (!fp) return null
  const same = matches(nodes, fp, within)
  const hit = same.length === 1 ? same[0] : same[fp.nth]
  return hit ? { ...hit, how: 'fp' } : null
}

function candidates(nodes: Node[], role?: string, within?: number) {
  const inside = (i: number) => { if (within === undefined) return true; for (let p = nodes[i].parent; p >= 0; p = nodes[p].parent) if (p === within) return true; return false }
  return nodes.map((n, i) => ({ n, i })).filter(({ n, i }) => ACTIVE.has(n.role) && (!role || role.split('|').includes(n.role)) && inside(i))
}

// target resolution: the saved fingerprint, exact AX name, contained AX name, then a jev choice whose top answer needs `bar` and a margin
function resolve(snap: AccessibilitySnapshot, nodes: Node[], target: string, role?: string, within?: number, bar = 0.5, fp?: Fp): Hit | null {
  const t0 = Date.now()
  const pool = candidates(nodes, role, within)
  const pinned = byFp(nodes, fp, within)
  if (pinned) { tick('ax', t0); return pinned }
  const want = norm(target)
  const covers = (n: Node) => {
    const nm = norm(n.name); if (!nm) return false
    const a = nm.split(' '), b = want.split(' ')
    return (` ${nm} `.includes(` ${want} `) && b.length * 2 >= a.length) || (a.length >= 2 && ` ${want} `.includes(` ${nm} `))
  }
  for (const f of [(n: Node) => norm(n.name) === want, covers]) {
    const hit = pool.filter(({ n }) => f(n))
    if (hit.length === 1) { tick('ax', t0); return { ...hit[0], how: 'ax' } }
  }
  const shown = pool.filter(({ n }) => { try { return snap.node(n.ref).isVisible !== false } catch { return false } })
  tick('ax', t0)
  if (!shown.length) return null
  const words = want.split(' ')
  const ranked = shown.map((c) => ({ c, s: words.filter((x) => norm(`${c.n.role} ${c.n.name} ${c.n.value}`).includes(x)).length })).sort((a, b) => b.s - a.s).slice(0, 40)
  const label = (c: { n: Node; i: number }) => {
    const sib = sibling(nodes, c.i); const par = c.n.parent >= 0 ? nodes[c.n.parent].name : ''
    return `${c.n.role} "${c.n.name}"${c.n.value ? ` value "${c.n.value}"` : ''}${sib ? ` labelled "${sib}"` : ''}${c.n.keys ? ` shortcut ${c.n.keys}` : ''}${par ? ` in "${par}"` : ''}`
  }
  const opts: Record<string, string | null> = { none: 'none of these elements' }
  for (const { c } of ranked) opts[`r${c.n.ref}`] = label(c)
  const d = decide(ranked.map(({ c }) => label(c)).join('\n'), { pick: choice(`Which element is "${target}"?`, opts) })
  const probs = Object.entries(d.pick.probabilities as Record<string, number>).sort((a, b) => b[1] - a[1])
  const [top, p1] = probs[0] ?? ['none', 0]; const p2 = probs[1]?.[1] ?? 0
  const hit = ranked.find(({ c }) => `r${c.n.ref}` === top)
  if (!hit || p1 < bar) return null
  if (p1 - p2 < MARGIN) {
    const near = probs.slice(0, 3).map(([k, p]) => `${k === 'none' ? 'none' : opts[k]} ${p.toFixed(2)}`).join('; ')
    throw new Surprise(`ambiguous target "${target}": ${near}; name the element more exactly`, seen())
  }
  return { ...hit.c, how: `jev(${p1.toFixed(2)})`, p: p1, margin: p1 - p2 }
}

function press(combo: string) {
  const keys = combo.split('+').map((k) => {
    const n = { ctrl: 'Control', control: 'Control', shift: 'Shift', alt: 'Alt', enter: 'Return', return: 'Return', esc: 'Escape', escape: 'Escape', tab: 'Tab', space: 'Space', del: 'Delete', delete: 'Delete', backspace: 'Backspace', up: 'UpArrow', down: 'DownArrow', left: 'LeftArrow', right: 'RightArrow', home: 'Home', end: 'End' }[k.toLowerCase()] ?? (k.length === 1 ? k.toUpperCase() : k)
    const key = (Key as any)[n]; if (key === undefined) throw new Error(`unknown key ${k}`); return key as Key
  })
  for (const k of keys.slice(0, -1)) m.key(k, Direction.Press)
  m.key(keys.at(-1)!, Direction.Click)
  for (const k of keys.slice(0, -1).reverse()) m.key(k, Direction.Release)
}

// a point the pointer can reach: a dialog taller than the screen reports controls below its edge
function reach(x: number, y: number) {
  screen ??= m.mainScreen().boundingBox()
  if (x < screen.left || y < screen.top || x >= screen.right || y >= screen.bottom) throw new Error(`(${x}, ${y}) is off screen`)
  return [x, y]
}

// the element's center when a hit-test there finds the element or a container that answers for its children (a GTK notebook page)
function point(snap: AccessibilitySnapshot, n: Node) {
  const b = snap.boundingBox(n.ref); const [x, y] = reach(b.left + b.width / 2, b.top + b.height / 2)
  const at = m.nodeAtPoint(x, y)?.overallDescription ?? ''
  if (!(n.name && norm(at.split(' under ')[0]).includes(norm(n.name))) && !HOLDERS.test(at)) throw new Error(`(${x}, ${y}) hits ${at.slice(0, 60)}`)
  return [x, y]
}

function act(snap: AccessibilitySnapshot, n: Node) {
  const t0 = Date.now()
  try {
    // a button's AT-SPI action runs its handler inside the a11y bridge's dispatch too, so a shown control takes a pointer click first
    const ways = [() => { const [x, y] = point(snap, n); m.moveMouse(x, y, Coordinate.Abs); m.mouseButton(Button.Left, Direction.Click) }, () => snap.activate(n.ref)]
    if (n.role.startsWith('menu')) ways.reverse()
    if (n.role === 'tab' || n.role === 'option' || n.role === 'radio') ways.unshift(() => snap.select(n.ref))
    for (const [k, f] of ways.entries()) { try { f(); return } catch (e) { if (k === ways.length - 1) throw e } }
  } finally { tick('act', t0) }
}

// an AT-SPI action on a menu entry runs its handler inside the a11y bridge's D-Bus dispatch: a modal dialog it opens answers no AT-SPI call, and the first key deadlocks the app
async function choose(snap: AccessibilitySnapshot, nodes: Node[], n: Node, rec: Record<string, unknown>, open = false) {
  const t0 = Date.now()
  try {
    const mods = (n.accel.match(/<\w+>/g) ?? []).map((x) => ({ '<Primary>': 'ctrl', '<Control>': 'ctrl', '<Shift>': 'shift', '<Alt>': 'alt' } as Record<string, string>)[x])
    const key = n.accel.replace(/<\w+>/g, '')
    if (key && mods.every(Boolean) && (mods.some((x) => x !== 'shift') || /^F\d+$/.test(key))) {
      if (open) for (let q = n.parent; q >= 0 && nodes[q].role === 'menu'; q = nodes[q].parent) press('Escape')
      try { press([...mods, key].join('+')); rec.via = 'shortcut'; return } catch {}
    }
    const shown = () => { try { const b = snap.boundingBox(n.ref); reach(b.left + b.width / 2, b.top + b.height / 2); return b.width > 0 ? b : null } catch { return null } }
    let b = shown()
    if (!b) {
      const chain: Node[] = []
      for (let q = n.parent; q >= 0 && nodes[q].role === 'menu'; q = nodes[q].parent) chain.unshift(nodes[q])
      for (const q of chain) snap.activate(q.ref)
      for (let i = 0; i < 10 && !(b = shown()); i++) await sleep(30)
    }
    if (b) { m.moveMouse(b.left + b.width / 2, b.top + b.height / 2, Coordinate.Abs); m.mouseButton(Button.Left, Direction.Click); rec.via = 'click'; return }
    snap.activate(n.ref); rec.via = 'action'
  } finally { tick('act', t0) }
}

async function settle(before: string | undefined) {
  const t0 = Date.now()
  for (let i = 0; i < 6 && focused()?.title === before; i++) await sleep(25)
  tick('settle', t0)
}

// a typed line that ends in a newline submits a file chooser: Return until it closes, since the first one may only close the completion popup
async function submit(w: NonNullable<ReturnType<typeof focused>>) {
  const t0 = Date.now()
  const [title, pid] = [w.title, pidOf(w)]
  const open = () => pid !== null && m.windows().some((x) => x.title === title && pidOf(x) === pid)
  for (let i = 0; i < 3 && open(); i++) {
    press('Return')
    for (let k = 0; k < 15 && open(); k++) await sleep(40)
  }
  tick('submit', t0)
  if (open() && focused()?.title === title) throw new Surprise(`"${title}" is still open after Return`, seen())
}

async function closed(title: string, pid: number | null) {
  const t0 = Date.now()
  const open = () => pid !== null && m.windows().some((x) => x.title === title && pidOf(x) === pid)
  for (let k = 0; k < 25 && open(); k++) await sleep(40)
  tick('settle', t0); return !open()
}

// the shown field a plan's first typing filled from empty, so a replay can check it is still empty before it types; window and tab names are no part of it
function shownFields(snap: AccessibilitySnapshot, nodes: Node[], fp: Pick<Fp, 'role' | 'name' | 'label'>) {
  return candidates(nodes, fp.role).filter(({ n, i }) => n.name === fp.name && (sibling(nodes, i) ?? '') === (fp.label ?? '') && (() => { try { return snap.node(n.ref).isVisible !== false } catch { return false } })())
}
function typedInto(w: NonNullable<ReturnType<typeof focused>>, text: string): Fp | undefined {
  const t0 = Date.now(); const squash = (v: string) => v.replace(/\s+/g, '')
  try {
    const snap = snapOf(w); const nodes = parse(snap)
    const hit = candidates(nodes, 'textbox|searchbox').find(({ n }) => { try { return snap.node(n.ref).isVisible !== false && squash(snap.node(n.ref).value ?? '') === squash(text) } catch { return false } })
    if (!hit) return undefined
    const fp: Fp = { role: hit.n.role, name: hit.n.name, parent: '', nth: 0 }
    const label = sibling(nodes, hit.i); if (label) fp.label = label
    fp.nth = shownFields(snap, nodes, fp).findIndex((x) => x.i === hit.i)
    return fp
  } catch { return undefined } finally { tick('blank', t0) }
}
function holdsText(w: NonNullable<ReturnType<typeof focused>>, fp: Fp) {
  const t0 = Date.now()
  try {
    const snap = snapOf(w); const same = shownFields(snap, parse(snap), fp); const hit = same.length === 1 ? same[0] : same[fp.nth]
    return !!hit && !!(snap.node(hit.n.ref).value ?? '').trim()
  } catch { return false } finally { tick('blank', t0) }
}

async function setState(s: Step, w: NonNullable<ReturnType<typeof focused>>, snap: AccessibilitySnapshot, hit: Hit, rec: Record<string, unknown>) {
  const on = s.on ?? true
  let was = await checked(w.pid, hit.n.name)
  if (was === null) was = cropState(snap, hit.n)
  rec.was = was
  if (was === on) { rec.noop = true; return }
  act(snap, hit.n)
  let now = await checked(w.pid, hit.n.name)
  for (let i = 0; i < 5 && now !== on && now !== null; i++) { await sleep(30); now = await checked(w.pid, hit.n.name) }
  if (now === null) now = cropState(snap, hit.n)
  if (now !== on) throw new Surprise(`"${hit.n.name}" is still ${now ? 'on' : 'off'}`, seen())
}

function cropState(snap: AccessibilitySnapshot, n: Node) {
  const b = snap.boundingBox(n.ref)
  const shot = m.screenshotCropped(Math.max(0, b.left - 40), Math.max(0, b.top - 4), b.width + 80, b.height + 8, true)
  return vision(`This is one control of a desktop app: "${n.name}". Is it switched on (a check mark, tick, filled box or pressed state)? {"on": true or false}`, shot).on === true
}

async function run(s: Step, rec: Record<string, unknown>) {
  if (s.app) {
    const before = new Set(m.windows().map((w) => w.title))
    let launched = false
    const has = () => m.windows().find((w) => norm(w.title).includes(norm(s.title ?? s.app!))) ?? (launched ? m.windows().find((w) => !before.has(w.title)) : undefined)
    let w = has()
    if (!w) {
      const t0 = Date.now(); launched = true
      try { m.fuzzyApp(s.app).open(null, FocusPolicy.Steal, Visibility.Show, true) } catch (e) {
        if (!/^[\w.-]+$/.test(s.app)) throw e
        spawn(s.app, [], { detached: true, stdio: 'ignore' }).unref()
      }
      for (const end = Date.now() + 8000; !(w = has()); await sleep(50)) if (Date.now() > end) throw new Surprise(`app "${s.app}" has no window`, seen())
      tick('launch', t0)
    }
    found = !launched; w.focus(); known.add(w.title); rec.window = lastSeen = w.title; await windowFor(w.title); return
  }
  if (s.type !== undefined) {
    const t0 = Date.now(); const w = focused(); const nl = s.type.endsWith('\n')
    const dialog = !s.type.includes('\n') && !!w && m.windows().some((x) => x.title !== w.title && pidOf(x) === pidOf(w))
    const o = (nl || dialog) && w ? await filled(w) : ''
    if (blocked(o)) throw new Surprise(unanswered(w!.title), seen())
    const chooser = o.includes('File Chooser Widget'); const sub = chooser && nl
    const first = !typed; typed = true
    if (s.blank && found && w && holdsText(w, s.blank)) throw new Surprise(`"${w.title}" already holds text where this flow typed into an empty field, and typing would add to it: open a new document (for example {key: "ctrl+n"}) or empty the field, then replay the flow`, seen())
    // GTK preselects the name up to its last dot: a bare name keeps the extension, a path or a name with its own extension replaces the whole field
    if (chooser && /\/|\.[a-z][a-z0-9]{0,7}\n?$/i.test(s.type)) { press('home'); press('shift+end') }
    // typeText sends "\n" as Linefeed, which GTK text views ignore: a line break goes as Return
    for (const [i, part] of (sub ? s.type.slice(0, -1) : s.type).split(/\r?\n/).entries()) { if (i) press('Return'); if (part) m.typeText(part) }
    tick('act', t0)
    if (recording && first && !chooser && w) rec.blank = typedInto(w, s.type)
    const v = nl && !sub ? focused() : null
    if (sub) { rec.submit = true; await submit(w!) } else if (v && isDialog(v.title === w?.title ? o : outline(v)) && await closed(v.title, pidOf(v))) rec.submit = true
    if (dialog && w) {
      const want = s.type.slice(0, 30).replace(/"/g, '\\"'); let after = ''
      for (let k = 0; k < 5; k++) { after = outline(w); if (!isDialog(after) || after.split('\n').some((l) => /^\s*- (textbox|searchbox|combobox|spinbutton)\b.*: ".*/.test(l) && l.includes(want))) return; await sleep(40) }
      throw new Surprise(`"${s.type}" reached no text field of "${w.title}" (Tab can land on a button): fill the field with {set: label, value}`, seen())
    }
    return
  }
  if (s.key) {
    const w = focused()
    const o = /^(enter|return)$/i.test(s.key) && w ? await filled(w) : ''
    if (blocked(o)) throw new Surprise(unanswered(w!.title), seen())
    if (o.includes('File Chooser Widget')) { rec.submit = true; await submit(w!); return }
    const t0 = Date.now(); press(s.key); tick('act', t0)
    if (isDialog(o) && await closed(w!.title, pidOf(w!))) rec.submit = true
    return
  }
  if (s.wait) { const w = await windowFor(s.wait, s.timeout_ms ?? 5000, s.seen); rec.seen = lastSeen = w.title; return }
  if (s.close) {
    // close shuts a dialog: the named window, else the focused dialog, else a dialog of the focused app; never an app's main window unless named
    const t0 = Date.now(); let w = s.window ? await windowFor(s.window, 3000, s.seen) : focused()
    const o = w && !s.window ? outline(w) : ''
    if (w && blocked(o) && m.windows().some((x) => pidOf(x) === pidOf(w!) && x.title !== w!.title)) { rec.seen = w.title; rec.how = 'wm'; w.close(); await settle(w.title); tick('act', t0); return }
    if (w && !s.window && !isDialog(o)) w = m.windows().find((x) => pidOf(x) === pidOf(w!) && x.title !== w!.title && isDialog(outline(x))) ?? null
    if (w && blocked(o)) throw new Surprise(unanswered(w.title), seen())
    if (!w) { rec.noop = true; tick('act', t0); return }
    const [title, pid] = [w.title, pidOf(w)]; rec.seen = title; w.focus(); press('Escape'); await settle(w.title)
    if (pid !== null && m.windows().some((x) => x.title === title && pidOf(x) === pid)) w.close()
    tick('act', t0); await closed(title, pid); return
  }
  const w = await windowFor(s.window, 3000, s.seen)
  rec.seen = w.title; if (w.title.trim()) lastSeen = w.title
  let t0 = Date.now(); let snap: AccessibilitySnapshot
  try { snap = snapOf(w) } catch (e) {
    if (blocked(String(e))) throw new Surprise(unanswered(w.title), seen())
    throw e
  }
  const nodes = parse(snap); tick('snap', t0)
  if (s.expect) {
    const d = decide({ window_title: w.title, contents: snap.print({ maxChars: 60 }).slice(0, 4000) }, { ok: noul(s.expect) })
    rec.p = +d.ok.probability.toFixed(2)
    if (d.ok.probability >= 0.5) return
    const shot = m.screenFromMouse().screenshot(true); shot.shrink(960, 600)
    const v = vision(`You check the state of a computer screen.\nCondition: ${s.expect}\n{"ok": true or false, "why": "one short sentence"}`, shot); rec.vision = v
    if (!v.ok) throw new Surprise(`expectation not met: ${s.expect} (${v.why})`, seen())
    return
  }
  if (s.verify) {
    const hit = resolve(snap, nodes, s.verify, s.role ?? (s.value !== undefined ? FIELDS : s.on !== undefined ? `${TOGGLES}|menuitem` : undefined), undefined, 0.7, s.fp)
    if (!hit) { if (s.on === false || s.enabled === false) return; throw new Surprise(`verify: no element matches "${s.verify}"`, seen()) }
    rec.how = hit.how; rec.fp = fingerprint(nodes, hit)
    const bad: string[] = []
    if (s.value !== undefined) { let v = ''; try { v = snap.node(hit.n.ref).value ?? '' } catch (e) { if (blocked(String(e))) throw new Surprise(unanswered(w.title), seen()); v = hit.n.value }; if (!norm(v).includes(norm(s.value))) bad.push(`reads "${v}"`) }
    if (s.enabled !== undefined && hit.n.disabled === s.enabled) bad.push(hit.n.disabled ? 'is disabled' : 'is enabled')
    if (s.on !== undefined) { const c = gtkToggle(w.pid, hit.n.name) ?? (await checked(w.pid, hit.n.name)) ?? cropState(snap, hit.n); rec.was = c; if (c !== s.on) bad.push(c ? 'is on' : 'is off') }
    if (bad.length) throw new Surprise(`verify: "${hit.n.name || s.verify}" ${bad.join(', ')}`, seen())
    return
  }
  if (s.menu) {
    let within: number | undefined
    let hit: Hit | null = null
    const parts = s.menu.split('>').map((x) => x.trim())
    const soft = (target: string, role: string | undefined, inside?: number) => { try { return resolve(snap, nodes, target, role, inside, 0.7) } catch (e) { if (e instanceof Surprise) return null; throw e } }
    hit = byFp(nodes, s.fp)
    if (!hit) {
      for (const [k, part] of parts.entries()) {
        hit = soft(part, k < parts.length - 1 ? 'menu' : undefined, within) ?? soft(part, undefined, within)
        if (!hit) {
          const leaf = resolve(snap, nodes, parts.at(-1)!, 'menuitem|menu|menuitemcheckbox|menuitemradio')
          if (leaf?.how !== 'ax') throw new Surprise(`menu entry "${part}" not found`, seen())
          hit = leaf; rec.how = 'ax-anywhere'; rec.path = [] as string[]
          for (let q = leaf.n.parent; q >= 0 && nodes[q].role === 'menu'; q = nodes[q].parent) (rec.path as string[]).unshift(nodes[q].name)
          break
        }
        within = hit.i; rec.how = hit.how
      }
    } else rec.how = 'fp'
    rec.fp = fingerprint(nodes, hit!)
    if (hit!.p !== undefined) rec.jev = [hit!.p, hit!.margin]
    if (s.on === undefined) { const pid = pidOf(w); await choose(snap, nodes, hit!.n, rec); await settle(w.title); const now = pid === null ? null : gtkToggle(pid, hit!.n.name); if (now !== null) rec.now = now; return }
    const was = gtkToggle(w.pid, hit!.n.name)
    if (was !== null) {
      rec.was = was; rec.state = 'gtk'
      if (was === s.on) { rec.noop = true; return }
      act(snap, hit!.n)
      let now = gtkToggle(w.pid, hit!.n.name)
      for (let i = 0; i < 5 && now !== s.on; i++) { await sleep(30); now = gtkToggle(w.pid, hit!.n.name) }
      if (now !== s.on) throw new Surprise(`menu toggle "${hit!.n.name}" is still ${now ? 'on' : 'off'}`, seen())
      return
    }
    const parent = hit!.n.parent >= 0 && nodes[hit!.n.parent].role === 'menu' ? nodes[hit!.n.parent] : null
    if (!parent) throw new Surprise(`"${hit!.n.name}" is not inside a menu`, seen())
    const chain: Node[] = []
    for (let q: Node | null = parent; q; q = q.parent >= 0 && nodes[q.parent].role === 'menu' ? nodes[q.parent] : null) chain.unshift(q)
    for (const q of chain) { act(snap, q); await sleep(120) }
    const on = cropState(snap, hit!.n); rec.was = on; rec.state = 'vision'
    if (on === s.on) { for (const _ of chain) press('Escape'); rec.noop = true; return }
    act(snap, hit!.n); await settle(w.title); return
  }
  const target = s.click ?? s.check ?? s.set!
  const hit = resolve(snap, nodes, target, s.role ?? (s.check ? TOGGLES : s.set ? FIELDS : undefined), undefined, s.set ? 0.5 : 0.7, s.fp)
  if (!hit) {
    if (!s.click) throw new Surprise(`no element matches "${target}"`, seen())
    t0 = Date.now()
    const [x, y] = m.screenFromMouse().screenshot(true).ground(GroundingModel.byAlias('gw_ground'), `${target} in the "${w.title}" window`)
    tick('ground', t0)
    const under = m.nodeAtPoint(x, y)
    const what = under?.overallDescription ?? ''
    rec.ground_hit = what.slice(0, 80)
    if (!what.trim() || decide({ element_under_point: what, window_title: w.title }, { is: noul(`Is this element "${target}"?`) }).is.probability < 0.7)
      throw new Surprise(`no element matches "${target}"${what.trim() ? `; grounding pointed at ${what.slice(0, 80)}` : ''}`, seen())
    t0 = Date.now(); m.moveMouse(x, y, Coordinate.Abs); m.mouseButton(Button.Left, Direction.Click); tick('act', t0)
    rec.how = 'ground'; rec.unsaved = true; await settle(w.title); return
  }
  rec.how = hit.how; rec.ref = `${hit.n.role} "${hit.n.name}"`; rec.fp = fingerprint(nodes, hit)
  if (hit.p !== undefined) rec.jev = [+hit.p.toFixed(2), +hit.margin!.toFixed(2)]
  if (hit.n.disabled) {
    t0 = Date.now(); let on = false
    for (let i = 0; i < 15 && !on; i++) { await sleep(40); on = !parse(snapOf(w)).find((n) => n.ref === hit.n.ref)?.disabled }
    tick('enable', t0)
    if (!on) throw new Surprise(`${rec.ref} is disabled`, seen())
  }
  if (s.click && hit.how.startsWith('jev') && TOGGLES.split('|').includes(hit.n.role))
    throw new Surprise(`"${target}" is closest to ${hit.n.role} "${hit.n.name}" (${hit.how}), and a click would flip it; use {check: "${hit.n.name}", on: ...} for it, or name the element you mean exactly`, seen())
  if (s.click && hit.n.role === 'menuitem') { await choose(snap, nodes, hit.n, rec, true); await settle(w.title); return }
  if (s.click) { act(snap, hit.n); await settle(w.title); return }
  if (s.check) { await setState(s, w, snap, hit, rec); return }
  const value = s.value!.endsWith('\n') ? s.value!.slice(0, -1) : s.value!
  const same = (v: string | undefined) => v === undefined || norm(v) === norm(value) || norm(v).includes(norm(value))
  const f = hit.n.role === 'combobox' ? nodes.find((x) => x.parent === hit.i && x.role === 'textbox') ?? hit.n : hit.n
  const lines = (snap.node(f.ref).value ?? '').includes('\n')
  if (hit.how.startsWith('jev') && lines) throw new Surprise(`"${target}" is closest to a multi-line text (${hit.how}), and set would replace all of it: wait for the window with that field, or name it exactly`, seen())
  t0 = Date.now()
  snap.setFocus(f.ref); if (f.role === 'textbox' && !lines) { press('home'); press('shift+end') } else press('ctrl+a'); m.typeText(value); if (hit.n.role === 'spinbutton' || hit.n.role === 'slider') { press('tab'); press('shift+tab') }
  tick('act', t0)
  t0 = Date.now(); let got: string | undefined
  for (let i = 0; i < 10; i++) { try { got = snap.node(f.ref).value } catch (e) { if (blocked(String(e))) throw new Surprise(unanswered(w.title), seen()); got = undefined }; if (same(got)) break; await sleep(40) }
  tick('verify', t0); rec.value = got
  if (!same(got)) throw new Surprise(`"${target}" reads "${got}" after set`, seen())
  if (s.value!.endsWith('\n')) { rec.submit = true; await submit(w) }
}

// {{name}} in any string of a step is a param
function fill(s: Step, p: Record<string, string>): Step {
  const out: any = {}
  for (const [k, v] of Object.entries(s)) out[k] = typeof v === 'string' ? v.replace(/\{\{(\w+)(:base)?\}\}/g, (all, n, b) => p[n] === undefined ? all : b ? p[n].split('/').at(-1)! : p[n]) : v
  return out
}

// values a later request would change become params: set values and typed text, unless the plan already templated them
function templated(steps: Step[], params: Record<string, string>) {
  const p = { ...params }
  const out = steps.map((s) => {
    const t = { ...s }
    for (const k of ['value', 'type'] as const) {
      const v = t[k]; if (v === undefined || /\{\{\w+\}\}/.test(v) || !v.replace(/\n$/, '')) continue
      const body = v.replace(/\n$/, ''); const have = Object.entries(p).find(([, x]) => x === body)
      let name = have?.[0] ?? (slug(k === 'value' ? t.set ?? 'value' : 'text').replace(/-/g, '_') || 'value')
      for (let n = 2; !have && name in p; n++) name = `${name.replace(/_\d+$/, '')}_${n}`
      p[name] = body; t[k] = `{{${name}}}${v.endsWith('\n') ? '\n' : ''}`
    }
    return t
  })
  for (const t of out) for (const k of ['wait', 'window'] as const) {
    let v = t[k]; if (!v) continue
    for (const [n, x] of Object.entries(p)) {
      if (x.length > 2 && v.includes(x)) v = v.replace(x, `{{${n}}}`)
      else if (x.includes('/') && x.split('/').at(-1)!.length > 2 && v.includes(x.split('/').at(-1)!)) v = v.replace(x.split('/').at(-1)!, `{{${n}:base}}`)
    }
    t[k] = v
  }
  return { steps: out, params: p }
}

function git(...args: string[]) { try { execFileSync('git', ['-C', FLOWS, ...args], { stdio: 'ignore', timeout: 5000 }) } catch {} }

function repo() {
  mkdirSync(FLOWS, { recursive: true })
  if (!existsSync(`${FLOWS}/.git`)) {
    git('init', '-q'); writeFileSync(`${FLOWS}/.gitignore`, '.runs.jsonl\n.pending.json\n')
    git('config', 'user.name', 'gui_plan'); git('config', 'user.email', 'gui_plan@box')
  }
}

function writeIndex() {
  const rows = readdirSync(FLOWS).filter((x) => x.endsWith('.json') && !x.startsWith('.')).map((x) => { try { return JSON.parse(readFileSync(`${FLOWS}/${x}`, 'utf8')) as Flow } catch { return null } })
    .filter((x): x is Flow => !!x?.name).sort((a, b) => b.saved_at.localeCompare(a.saved_at))
  writeFileSync(INDEX, rows.map((x) => `${x.name}(${Object.keys(x.params).join(', ')})${x.idempotent ? '' : ' once'}: ${summary(x.steps)}`).join('\n') + '\n')
}

function saveFlow(f: Flow, why: string) {
  repo()
  writeFileSync(`${FLOWS}/${f.name}.json`, JSON.stringify(f, null, 1) + '\n')
  writeIndex()
  git('add', '-A'); git('commit', '-q', '-m', `${f.name}: ${why}`)
}

// what a flow does, in its targets: "Edit > Preferences, Use system monospace font off, Size={{size}}"
function summary(steps: Step[]) {
  const out = steps.filter((s) => !s.app && !s.wait && !s.close && !s.verify).map((s) => s.menu ?? (s.check ? `${s.check} ${s.on === false ? 'off' : 'on'}` : s.set ? `${s.set}=${s.value}` : s.type !== undefined ? `type ${s.type.replace(/\n$/, '')}` : s.key ? `key ${s.key}` : s.click))
  const text = out.join(', ')
  return text.length > 100 ? `${text.slice(0, 99)}…` : text
}

const idempotent = (steps: Step[]) => steps.every((s) => s.type === undefined && (!s.key || /^(esc|escape|tab|shift\+tab)$/i.test(s.key)))
const bare = (s: Step, p: Record<string, string>) => JSON.stringify(Object.entries(fill(s, p)).filter(([k]) => k !== 'fp' && k !== 'seen' && k !== 'blank').sort())

// a chain becomes the flow it names, or one named after its app and last targets
function store(chain: Chain) {
  const tpl = templated(chain.steps, chain.params)
  const keys: Record<string, string> = { 'ctrl+n': 'new', 'ctrl+s': 'save', 'ctrl+shift+s': 'save as', 'ctrl+o': 'open' }
  const targets = chain.steps.filter((s) => s.menu || s.check || s.set || s.click || keys[s.key?.toLowerCase() ?? '']).slice(-2).map((s) => s.key ? keys[s.key.toLowerCase()] : (s.menu ?? s.check ?? s.set ?? s.click)!.split('>').at(-1))
  const auto = slug([chain.steps[0].app, ...targets].filter(Boolean).map((x) => x!.split(' ').slice(0, 3).join(' ')).join(' ')) || undefined
  if (!chain.named && auto && chain.name && auto !== chain.name) git('rm', '-q', '--ignore-unmatch', `${chain.name}.json`)
  if (!chain.named) chain.name = auto
  if (chain.name) saveFlow({ name: chain.name, params: tpl.params, steps: tpl.steps, idempotent: idempotent(chain.steps), runs: 1, saved_at: new Date().toISOString(), rewrites: 0 }, `saved from ${chain.steps.length} passing steps`)
  return chain.name
}

// {save} alone saves the last plan without running it again; after an escalation, its passed steps while their window is still in front
function promote(name: string, params: Record<string, string> = {}, rerun = false) {
  let c: Chain | null = null
  try { c = JSON.parse(readFileSync(PENDING, 'utf8')) } catch {}
  if (!c || Date.now() - c.at >= CHAIN_AGE || !(c.saved || c.escalated) || c.dirty || !c.steps.some((s) => !s.app) || !slug(name))
    return { ok: false, reason: 'nothing to save: the last gui_plan run of the last 10 minutes must start at an app step, pass, and click no target found by vision' }
  if (c.escalated || rerun) {
    const f = focused(); const o = f ? outline(f) : ''
    if (!f || f.title !== c.window || blocked(o) || isDialog(o)) return { ok: false, reason: `the last plan stopped at a surprise and "${f?.title ?? 'no window'}" is in front: continue it with gui_plan from the failing step, and the passing run is saved` }
  }
  const old = c.saved && !c.named ? c.name : undefined
  c.name = slug(name); c.named = true; c.params = { ...c.params, ...params }
  if (old && old !== c.name) git('rm', '-q', '--ignore-unmatch', `${old}.json`)
  store(c); c.saved = true; c.escalated = false
  try { writeFileSync(PENDING, JSON.stringify(c)) } catch {}
  return { ok: true, saved: c.name, params: templated(c.steps, c.params).params, ...(idempotent(c.steps) ? {} : { note: 'a replay of this flow types again' }) }
}

function loadFlow(name: string): Flow {
  const path = `${FLOWS}/${slug(name)}.json`
  if (!existsSync(path)) throw new Surprise(`no saved flow "${name}"; saved: ${existsSync(INDEX) ? readFileSync(INDEX, 'utf8').trim().split('\n').join(', ') : 'none'}`)
  return JSON.parse(readFileSync(path, 'utf8'))
}

function find(request: string) {
  const rows = existsSync(INDEX) ? readFileSync(INDEX, 'utf8').trim().split('\n').filter(Boolean) : []
  if (!rows.length) return { ok: false, flows: [] }
  const opts: Record<string, string | null> = { none: 'none of these flows does it' }
  rows.forEach((r, i) => { opts[`f${i}`] = r })
  const d = decide({ request }, { pick: choice('Which saved GUI flow does this request?', opts) })
  const k = d.pick.choice as string; const p = d.pick.probabilities[k] as number
  const next = Object.values(d.pick.probabilities as Record<string, number>).sort((a, b) => b - a)[1] ?? 0
  if (k === 'none' || p < 0.6 || p - next < MARGIN) return { ok: false, flow: null, p: +p.toFixed(2) }
  return { ok: true, flow: rows[+k.slice(1)].replace(/\(.*$/, ''), p: +p.toFixed(2) }
}

export async function main(p: { steps?: Step[]; flow?: string; params?: Record<string, string>; save?: string; start?: number; find?: string; reindex?: boolean }) {
  const t0 = Date.now()
  if (p.reindex) { repo(); writeIndex(); git('add', '-A'); git('commit', '-q', '-m', 'sync from the flow library'); return { ok: true } }
  if (p.find) return find(p.find)
  if (p.save && !p.steps?.length && !p.flow) return promote(p.save)
  let flow: Flow | null = null
  try { if (p.flow) flow = loadFlow(p.flow) } catch (e) { return { ok: false, done: 0, ms: 0, escalate: { reason: (e as Error).message } } }
  const params = { ...(flow?.params ?? {}), ...(p.params ?? {}) }
  const plan: Step[] = (flow ? flow.steps : p.steps ?? []).filter((s) => !Object.keys(s).every((k) => ['screenshot', 'timeout_s', 'max_edge'].includes(k)))
  if (!plan.length) return { ok: false, error: 'pass steps, flow or find' }
  recording = !flow && !!plan[0].app && !p.start
  // a plan sent again only to save it, while the window it left is still in front, would type again: it is saved from the last run instead
  if (p.save && !flow && !p.start && !idempotent(plan)) {
    let c: Chain | null = null
    try { c = JSON.parse(readFileSync(PENDING, 'utf8')) } catch {}
    const was = c?.steps.filter((s) => !s.close) ?? []; const now = plan.filter((s) => !s.close)
    if (c && was.length === now.length && was.every((s, i) => bare(s, c.params) === bare(now[i], params))) {
      const r = promote(p.save, p.params, true)
      if (r.ok) return { ...r, ran: false, why: 'these steps passed in the last plan, so they were saved without running them again; call without save to run them' }
    }
  }
  const trace: Record<string, unknown>[] = []; const log: Record<string, unknown>[] = []
  for (const w of m.windows()) known.add(w.title)
  const startTitles = new Set(known)
  const startWindow = focused()?.title ?? null
  const start = focused(); const startPid = start ? pidOf(start) : null
  const startAlert = !focused()?.title.trim() && isAlert(outline())
  const recs: Record<string, unknown>[] = []
  let escalate: Record<string, unknown> | null = null; let done = plan.length
  const bad = plan.findIndex((s) => OPS.filter((k) => k in s).length !== 1)
  if (bad >= 0) return { ok: false, done: 0, ms: 0, trace, escalate: { step: bad, plan_step: plan[bad], reason: `step ${bad} needs exactly one op of ${OPS.join(', ')}; nothing ran` } }
  const unfilled = (s: Step) => Object.values(s).flatMap((v) => typeof v === 'string' ? [...v.matchAll(/\{\{(\w+)(?::base)?\}\}/g)].map((x) => x[1]) : []).find((n) => params[n] === undefined)
  const gap = plan.findIndex((s, i) => i >= (p.start ?? 0) && unfilled(s))
  if (gap >= 0) return { ok: false, done: 0, ms: 0, trace, escalate: { step: gap, plan_step: plan[gap], reason: `no value for {{${unfilled(plan[gap])}}}: pass it in params; nothing ran` } }
  for (let i = p.start ?? 0; i < plan.length; i++) {
    const s = fill(plan[i], params); T = {}; const rec: Record<string, unknown> = { i, op: OPS.find((k) => k in s) }
    const ts = Date.now()
    try {
      const f = focused()
      const o = f && (!known.has(f.title) || !f.title.trim()) && !(i === (p.start ?? 0) && startAlert) && !(s.window && norm(f.title).includes(norm(s.window))) ? outline(f) : ''
      if (blocked(o)) throw new Surprise(unanswered(f!.title), seen())
      if (isAlert(o)) throw new Surprise(`alert "${f!.title}" is in front`, seen())
      await run(s, rec)
    } catch (e) {
      const sp = e instanceof Surprise ? e : new Surprise(String((e as Error).message ?? e), seen())
      escalate = { step: i, plan_step: plan[i], reason: sp.reason, ...sp.seen }; done = i
    }
    rec.ms = Date.now() - ts; log.push({ ...rec, ...T }); recs[i] = rec
    trace.push({ i, op: rec.op, how: rec.how, ms: rec.ms, ...(rec.was !== undefined ? { was: rec.was ? 'on' : 'off', now: rec.noop || s.verify ? undefined : s.on === false ? 'off' : 'on' } : {}), ...(rec.now !== undefined ? { now: rec.now ? 'on' : 'off' } : {}), ...(rec.noop ? { noop: true } : {}) })
    if (escalate) break
  }
  const last = plan.at(-1)!
  let opened = false
  if (!escalate) {
    const ts = Date.now(); const was = focused()?.title
    if ((last.key || last.type !== undefined || last.click) && !recs.at(-1)?.submit) {
      for (let i = 0; i < 12 && focused()?.title === was; i++) await sleep(40)
      await sleep(80)
    }
    const f = focused(); const o = f ? outline(f) : ''
    log.push({ i: plan.length, op: 'end', ms: Date.now() - ts, window: f?.title ?? null })
    const left = f && !startTitles.has(f.title) && isDialog(o) && !(last.wait || last.app || (last.window && norm(f.title).includes(norm(last.window))))
    if (f && blocked(o) && recs.at(-1)?.how !== 'wm') { escalate = { step: plan.length, reason: unanswered(f.title), ...seen() }; done = plan.length }
    else if (f && (isAlert(o) || left)) { escalate = { step: plan.length, reason: `the plan ended with "${f.title || o.split('\n')[0]}" still in front`, ...seen() }; done = plan.length }
    opened = !!f && !startTitles.has(f.title) && isDialog(o)
  }
  let saved: string | undefined
  const jevs = recs.filter((r) => r?.jev).length
  const settled = !escalate && !opened
  if (flow) {
    try { writeFileSync(PENDING, 'null') } catch {}
    if (settled && recs.every((r) => !r?.unsaved)) {
      const rewrites = recs.filter((r, i) => r?.fp && JSON.stringify(r.fp) !== JSON.stringify(flow.steps[i].fp))
      flow.runs += 1
      if (rewrites.length) {
        recs.forEach((r, i) => { if (r?.fp) flow.steps[i].fp = r.fp as Fp; if (r?.seen) flow.steps[i].seen = r.seen as string })
        flow.rewrites += 1; flow.saved_at = new Date().toISOString(); saveFlow(flow, `replay re-resolved ${rewrites.length} step(s)`)
      }
      saved = flow.name
    }
  } else {
    // a task done over several calls is one chain: it starts at a call whose first step is an app, and later calls extend it from the window it left
    const ran = plan.slice(p.start ?? 0, done).map((s, k) => { const r = recs[(p.start ?? 0) + k]; const o: Step = { ...s }; if (r?.fp) o.fp = r.fp as Fp; if (r?.seen && !s.app) o.seen = r.seen as string; if (r?.blank) o.blank = r.blank as Fp; return o })
    const dirty = recs.some((r) => r?.unsaved)
    let chain: Chain | null = null; let prev: Chain | null = null
    try { prev = JSON.parse(readFileSync(PENDING, 'utf8')) } catch {}
    if (plan[0].app && !(p.start ?? 0)) chain = { at: Date.now(), window: null, pid: null, params: { ...(p.params ?? {}) }, steps: ran, dirty }
    else if (prev && Date.now() - prev.at < CHAIN_AGE && (prev.window === startWindow || (prev.pid !== null && prev.pid === startPid))) chain = { ...prev, at: Date.now(), params: { ...prev.params, ...(p.params ?? {}) }, steps: [...prev.steps, ...ran], dirty: prev.dirty || dirty }
    if (chain) {
      if (p.save) { chain.name = slug(p.save); chain.named = true }
      const changes = chain.steps.some((s) => s.check || s.set || s.type !== undefined || (s.menu && s.on !== undefined))
      chain.escalated = !!escalate; chain.saved = false
      if ((settled || (!escalate && changes)) && !chain.dirty && chain.steps.some((s) => !s.app)) { saved = store(chain); chain.saved = !!saved }
      const end = focused()
      chain.window = end?.title ?? null
      if (end && !/^(Desktop|xfce4-panel)$/.test(end.title)) chain.pid = pidOf(end)
      try { mkdirSync(FLOWS, { recursive: true }); writeFileSync(PENDING, JSON.stringify(chain)) } catch {}
    } else if (prev) try { writeFileSync(PENDING, JSON.stringify({ ...prev, saved: false, escalated: false })) } catch {}
  }
  const ms = Date.now() - t0
  try { mkdirSync(FLOWS, { recursive: true }); appendFileSync(LOG, JSON.stringify({ at: new Date().toISOString(), flow: p.flow ?? null, ok: !escalate, ms, jevs, saved: saved ?? null, steps: log }) + '\n') } catch {}
  const out: Record<string, unknown> = { ok: !escalate, done, ms, trace }
  if (saved) out[flow ? 'flow' : 'saved'] = saved
  if (escalate) out.escalate = escalate
  else out.window = focused()?.title ?? null
  return out
}
