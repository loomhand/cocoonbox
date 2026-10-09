// press.cjs: activate on a shown button or menu item clicks it with the pointer, since its AT-SPI action runs the handler inside the
// a11y bridge's D-Bus dispatch, where a modal dialog it opens blocks the app and the first key deadlocks it; provision.sh loads it from wrapped.js
'use strict'
const HOLDERS = /^at-spi\.role\.(page|filler|panel|scroll|viewport|layered|frame|dialog|window)\b/
const norm = (s) => s.toLowerCase().replace(/[_…]|\.\.\.|:$/g, '').replace(/\s+/g, ' ').trim()
const nap = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms)
// the menus that hold entry i of a snapshot, outermost first, read from the indentation of its outline
function menusOf(s, i) {
  const rows = [...s.print({ maxChars: 8 }).matchAll(/^( *)- (\S+).*?\[ref=(\d+)\]/gm)].map((g) => ({ depth: g[1].length / 2, role: g[2], ref: +g[3] }))
  const out = []
  let k = rows.findIndex((r) => r.ref === i)
  for (let d = rows[k]?.depth ?? 0; --k >= 0 && d > 0;) if (rows[k].depth < d) { d = rows[k].depth; if (rows[k].role !== 'menu') break; out.unshift(rows[k].ref) }
  return out
}

module.exports = (sl) => {
  const PRESS = new Set([sl.AriaRole.Button, sl.AriaRole.MenuItem])
  // a class with statics is exported as a subclass; its instances carry the napi class's prototype
  const proto = (k) => (Object.getOwnPropertyNames(k.prototype).length > 1 ? k : Object.getPrototypeOf(k)).prototype
  const tree = proto(sl.AccessibilityTree), snap = proto(sl.AccessibilitySnapshot), node = proto(sl.AccessibilityNode)
  const { activate: treeActivate, find, findByDescription } = tree
  const { activate: snapActivate, click: snapClick } = snap
  const { activate: nodeActivate } = node
  const refs = new WeakMap()
  let m = null, screen = null

  // the element's center when it is on the screen and a hit-test there finds it by name, a container that answers for its children
  // (a GTK notebook page), or a node with its bounds; an element of unknown role passes only the last, as a button or menu item
  function point(b, el) {
    m ??= sl.Machine.local(); screen ??= m.mainScreen().boundingBox()
    const [x, y] = [b.left + b.width / 2, b.top + b.height / 2]
    if (!(b.width > 0) || x < screen.left || y < screen.top || x >= screen.right || y >= screen.bottom) return null
    const at = m.nodeAtPoint(x, y)
    const desc = at?.overallDescription ?? ''
    if (el && ((el.name && norm(desc.split(' under ')[0]).includes(norm(el.name))) || HOLDERS.test(desc))) return [x, y]
    for (let n = at; n; n = n.parent()) {
      const nb = n.boundingBox()
      if (nb.left === b.left && nb.top === b.top && nb.width === b.width && nb.height === b.height) return el || PRESS.has(n.role) ? [x, y] : null
      if (nb.left < b.left || nb.top < b.top || nb.left + nb.width > b.left + b.width || nb.top + nb.height > b.top + b.height) return null
    }
    return null
  }
  // `who` gives the element's {role, name}, or null when unknown (a tree ref not returned by find); `open` posts the closed menus of a menu entry
  function press(who, bounds, action, open) {
    let at = null
    try {
      const el = who()
      if (!el || PRESS.has(el.role)) at = point(bounds(), el)
      if (!at && el?.role === sl.AriaRole.MenuItem && open()) for (let i = 0; i < 10 && !(at = point(bounds(), el)); i++) nap(30)
    } catch {}
    if (!at) return action()
    m.moveMouse(at[0], at[1], sl.Coordinate.Abs)
    m.mouseButton(sl.Button.Left, sl.Direction.Click)
  }

  const remember = (t, nodes) => { refs.set(t, new Map(nodes.filter((n) => n.refId != null).map((n) => [n.refId, { role: n.role, name: n.name }]))); return nodes }
  tree.find = function (...a) { return remember(this, find.apply(this, a)) }
  tree.findByDescription = function (...a) { return remember(this, findByDescription.apply(this, a)) }
  // posting a menu runs no nested main loop, so its AT-SPI action is safe
  const post = (s, i) => { const menus = menusOf(s, i); for (const q of menus) snapActivate.call(s, q); return menus.length > 0 }
  // a tree ref has no node: find the entry in a snapshot of the tree's window by its name
  function postByName(t, name) {
    const w = m.windows().find((x) => x.title === t.windowTitle); if (!w) return false
    const s = sl.AccessibilitySnapshot.fromWindow(w)
    const hits = [...s.print({ maxChars: 8 }).matchAll(/- menuitem\b.*?\[ref=(\d+)\]/g)].map((g) => +g[1]).filter((i) => s.node(i).name === name)
    return hits.length === 1 && post(s, hits[0])
  }
  tree.activate = function (ref) {
    const el = () => refs.get(this)?.get(ref) ?? null
    return press(el, () => this.getBounds(ref), () => treeActivate.call(this, ref), () => postByName(this, el().name))
  }
  const live = (n) => ({ role: n.role, name: n.name })
  snap.activate = function (i) { return press(() => live(this.node(i)), () => this.boundingBox(i), () => snapActivate.call(this, i), () => post(this, i)) }
  snap.click = function (i) { return press(() => live(this.node(i)), () => this.boundingBox(i), () => snapClick.call(this, i), () => post(this, i)) }
  node.activate = function () {
    const open = () => {
      const menus = []
      for (let p = this.parent(); p && p.role === sl.AriaRole.Menu; p = p.parent()) menus.unshift(p)
      for (const p of menus) nodeActivate.call(p)
      return menus.length > 0
    }
    return press(() => live(this), () => this.boundingBox(), () => nodeActivate.call(this), open)
  }
}
