// press.cjs: activate on a shown button or menu item clicks it with the pointer, since its AT-SPI action runs the handler inside the
// a11y bridge's D-Bus dispatch, where a modal dialog it opens blocks the app and the first key deadlocks it; provision.sh loads it from wrapped.js
'use strict'
const HOLDERS = /^at-spi\.role\.(page|filler|panel|scroll|viewport|layered|frame|dialog|window)\b/
const norm = (s) => s.toLowerCase().replace(/[_…]|\.\.\.|:$/g, '').replace(/\s+/g, ' ').trim()

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
  // `who` gives the element's {role, name}, or null when unknown (a tree ref not returned by find)
  function press(who, bounds, action) {
    let at = null
    try { const el = who(); if (!el || PRESS.has(el.role)) at = point(bounds(), el) } catch {}
    if (!at) return action()
    m.moveMouse(at[0], at[1], sl.Coordinate.Abs)
    m.mouseButton(sl.Button.Left, sl.Direction.Click)
  }

  const remember = (t, nodes) => { refs.set(t, new Map(nodes.filter((n) => n.refId != null).map((n) => [n.refId, { role: n.role, name: n.name }]))); return nodes }
  tree.find = function (...a) { return remember(this, find.apply(this, a)) }
  tree.findByDescription = function (...a) { return remember(this, findByDescription.apply(this, a)) }
  tree.activate = function (ref) { return press(() => refs.get(this)?.get(ref) ?? null, () => this.getBounds(ref), () => treeActivate.call(this, ref)) }
  const live = (n) => ({ role: n.role, name: n.name })
  snap.activate = function (i) { return press(() => live(this.node(i)), () => this.boundingBox(i), () => snapActivate.call(this, i)) }
  snap.click = function (i) { return press(() => live(this.node(i)), () => this.boundingBox(i), () => snapClick.call(this, i)) }
  node.activate = function () { return press(() => live(this), () => this.boundingBox(), () => nodeActivate.call(this)) }
}
