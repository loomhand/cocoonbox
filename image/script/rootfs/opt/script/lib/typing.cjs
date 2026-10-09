// typing.cjs: typeText lets the focused app catch up before Enigo rebinds its spare keycodes. Enigo types a character missing from
// the keymap's first column through a spare keycode and, once all are bound, rebinds them at once, so keys the app has not read yet
// come out as other characters; provision.sh loads it from wrapped.js
'use strict'
const { execFileSync } = require('node:child_process')

const DRAIN_MS = 150
const READ_AT = 8
const US_FIRST = new Set("abcdefghijklmnopqrstuvwxyz0123456789`-=[]\\;',./ \t\n\r\b\x1b\x7f")
const CONTROL = { '\b': 0xff08, '\t': 0xff09, '\n': 0xff0a, '\v': 0xff0b, '\r': 0xff0d, '\x1b': 0xff1b, '\x7f': 0xffff }

module.exports = (sl) => {
  const k = sl.Machine
  const proto = (Object.getOwnPropertyNames(k.prototype).length > 1 ? k : Object.getPrototypeOf(k)).prototype
  const { typeText } = proto
  const pause = new Int32Array(new SharedArrayBuffer(4))
  const bound = new Set()
  let keymap = null

  // the first-column keysyms, and the keycodes without a keysym but keycode 8: what Enigo reads when it connects
  function read() {
    const first = new Set()
    let spare = 0
    for (const line of execFileSync('xmodmap', ['-pk'], { encoding: 'utf8' }).split('\n')) {
      const m = /^\s+(\d+)\s+(0x[0-9a-f]+)?/.exec(line)
      if (m?.[2]) first.add(parseInt(m[2], 16))
      else if (m && m[1] !== '8') spare++
    }
    return { first, spare }
  }
  const keysym = (c) => {
    const cp = c.codePointAt(0)
    return CONTROL[c] ?? ((cp >= 0x20 && cp < 0x7f) || (cp >= 0xa0 && cp < 0x100) ? cp : cp | 0x01000000)
  }

  proto.typeText = function (text) {
    // until a run binds READ_AT characters it cannot reach the spares the toolhost leaves free, so it counts by the us layout
    if (!keymap) {
      const fresh = new Set([...text].filter((c) => !US_FIRST.has(c) && !bound.has(c)))
      if (bound.size + fresh.size < READ_AT) {
        for (const c of fresh) bound.add(c)
        return typeText.call(this, text)
      }
      keymap = read()
      for (const c of bound) if (keymap.first.has(keysym(c))) bound.delete(c)
    }
    let from = 0
    let at = 0
    for (const c of text) {
      if (!keymap.first.has(keysym(c)) && !bound.has(c)) {
        if (bound.size >= keymap.spare) {
          typeText.call(this, text.slice(from, at))
          Atomics.wait(pause, 0, 0, DRAIN_MS)
          bound.clear()
          from = at
        }
        bound.add(c)
      }
      at += c.length
    }
    return typeText.call(this, text.slice(from))
  }
}
