package edge

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync/atomic"
)

const (
	stageVersion = iota
	stageSecurity
	stageResult
	stageInit
	stageMessages
	stageFailed

	rfbVersionLen   = 12
	rfbSecurityNone = 1
	rfbShared       = 1
	rfbBitsPerPixel = 32
	rfbPixelBytes   = rfbBitsPerPixel / 8
	rfbServerInit   = 24
	maxFencePayload = 64
	maxRFBBuffer    = 1 << 20

	encodingRaw          = 0
	encodingCopyRect     = 1
	encodingZRLE         = 16
	encodingDesktopSize  = -223
	encodingLastRect     = -224
	encodingCursor       = -239
	encodingExtendedSize = -308
	encodingFence        = -312
	encodingContinuous   = -313
)

var (
	rfbVersions = map[string]int32{"RFB 003.003\n": 3, "RFB 003.007\n": 7, "RFB 003.008\n": 8}

	// rfbFixedLen sizes the client messages of a fixed length: the framebuffer ones a viewer keeps, then the input it loses.
	rfbFixedLen = map[byte]int{0: 20, 3: 10, 150: 10, 4: 8, 5: 6, 250: 4}
	// rfbHeaderLen is how much of a variable-length message names its length.
	rfbHeaderLen = map[byte]int{2: 4, 6: 8, 248: 9, 251: 8, 255: 4}
	rfbKept      = map[byte]bool{0: true, 2: true, 3: true, 150: true, 248: true}

	// viewerEncodings are the encodings a viewer may ask for: those whose rects the bridge can frame without decoding pixels.
	viewerEncodings = []int32{
		encodingRaw, encodingCopyRect, encodingZRLE, encodingDesktopSize, encodingLastRect,
		encodingCursor, encodingExtendedSize, encodingFence, encodingContinuous,
	}
)

// rfbSession is what the two directions of one bridged RFB stream share: the protocol minor version the viewer chose.
type rfbSession struct {
	minor atomic.Int32
}

// viewOnly filters the client side of an RFB stream: it passes the handshake, a 32-bit pixel format, the encodings the
// bridge can frame, and framebuffer requests; it forces a shared session and drops key, pointer, clipboard, resize, xvp
// and QEMU messages; anything else ends the stream.
type viewOnly struct {
	session *rfbSession
	stage   int
	buf     []byte
	// skip counts bytes of a dropped message that have not arrived yet.
	skip int
}

// feed takes the next bytes from the viewer and returns those to forward to the server.
func (f *viewOnly) feed(in []byte) ([]byte, error) {
	n := min(f.skip, len(in))
	f.skip -= n
	f.buf = append(f.buf, in[n:]...)
	var out []byte
	for len(f.buf) > 0 {
		size, forward, err := f.next()
		if err != nil {
			return nil, err
		}
		if size == 0 {
			break
		}
		if size > len(f.buf) {
			f.skip = size - len(f.buf)
			f.buf = f.buf[:0]
			break
		}
		out = append(out, forward...)
		f.buf = f.buf[size:]
	}
	if len(f.buf) > maxRFBBuffer {
		return nil, errors.New("rfb: viewer message too large")
	}
	f.buf = append([]byte(nil), f.buf...)
	return out, nil
}

// next sizes the unit at the head of buf and returns what to forward of it; size 0 waits for more bytes, and a size past
// the buffer is only ever returned for a dropped message.
func (f *viewOnly) next() (int, []byte, error) {
	b := f.buf
	switch f.stage {
	case stageVersion:
		if len(b) < rfbVersionLen {
			return 0, nil, nil
		}
		minor := rfbVersions[string(b[:rfbVersionLen])]
		if minor == 0 {
			return 0, nil, fmt.Errorf("rfb: unsupported version %q", b[:rfbVersionLen])
		}
		f.session.minor.Store(minor)
		f.stage = stageSecurity
		if minor == 3 {
			f.stage = stageInit
		}
		return rfbVersionLen, b[:rfbVersionLen], nil
	case stageSecurity:
		if b[0] != rfbSecurityNone {
			return 0, nil, fmt.Errorf("rfb: security type %d is not None", b[0])
		}
		f.stage = stageInit
		return 1, b[:1], nil
	case stageInit:
		b[0] = rfbShared
		f.stage = stageMessages
		return 1, b[:1], nil
	}
	size, err := messageLen(b)
	if err != nil || !rfbKept[b[0]] {
		return size, nil, err
	}
	if size > len(b) {
		return 0, nil, nil
	}
	switch b[0] {
	case 0:
		if b[4] != rfbBitsPerPixel {
			return 0, nil, fmt.Errorf("rfb: pixel format of %d bits; the bridge frames only %d", b[4], rfbBitsPerPixel)
		}
	case 2:
		return size, viewerSetEncodings(b[:size]), nil
	}
	return size, b[:size], nil
}

// viewerSetEncodings rewrites a SetEncodings message to the encodings in viewerEncodings, in the viewer's order.
func viewerSetEncodings(msg []byte) []byte {
	out := []byte{2, 0, 0, 0}
	kept := 0
	for i := 4; i+4 <= len(msg); i += 4 {
		if slices.Contains(viewerEncodings, int32(binary.BigEndian.Uint32(msg[i:i+4]))) { //nolint:gosec // encodings are signed
			out = append(out, msg[i:i+4]...)
			kept++
		}
	}
	binary.BigEndian.PutUint16(out[2:4], uint16(kept))
	return out
}

// messageLen sizes the client message at the head of b, or returns 0 until its header has arrived.
func messageLen(b []byte) (int, error) {
	kind := b[0]
	if n, ok := rfbFixedLen[kind]; ok {
		return n, nil
	}
	header := rfbHeaderLen[kind]
	if header == 0 {
		return 0, fmt.Errorf("rfb: unsupported client message %d", kind)
	}
	if len(b) < header {
		return 0, nil
	}
	switch kind {
	case 2:
		return 4 + 4*int(binary.BigEndian.Uint16(b[2:4])), nil
	case 6:
		return cutTextLen(b)
	case 248:
		if b[8] > maxFencePayload {
			return 0, errors.New("rfb: fence payload over 64 bytes")
		}
		return 9 + int(b[8]), nil
	case 251:
		return 8 + 16*int(b[6]), nil
	}
	switch qemu := b[1]; {
	case qemu == 0:
		return 12, nil
	case qemu == 1 && binary.BigEndian.Uint16(b[2:4]) == 2:
		return 10, nil
	case qemu == 1:
		return 4, nil
	default:
		return 0, fmt.Errorf("rfb: unsupported QEMU message %d", qemu)
	}
}

// screenOnly filters the server side of an RFB stream for a viewer: it frames the handshake, framebuffer updates rect by
// rect, color maps, bells, fences and the end of continuous updates without decoding pixels, and drops the clipboard
// (ServerCutText, plain and extended); any other message or encoding ends the stream.
type screenOnly struct {
	session *rfbSession
	stage   int
	buf     []byte
	// rects counts the rects left in the current FramebufferUpdate.
	rects int
	// pass and skip count body bytes still to forward or to drop.
	pass int
	skip int
}

// feed takes the next bytes from the server and returns those to forward to the viewer.
func (f *screenOnly) feed(in []byte) ([]byte, error) {
	f.buf = append(f.buf, in...)
	out := make([]byte, 0, len(in))
	pos := 0
	for {
		n := min(f.pass, len(f.buf)-pos)
		out = append(out, f.buf[pos:pos+n]...)
		f.pass -= n
		pos += n
		n = min(f.skip, len(f.buf)-pos)
		f.skip -= n
		pos += n
		if f.pass > 0 || f.skip > 0 || pos == len(f.buf) {
			break
		}
		head, body, keep, err := f.next(f.buf[pos:])
		if err != nil {
			return nil, err
		}
		if head == 0 {
			break
		}
		if keep {
			out = append(out, f.buf[pos:pos+head]...)
			f.pass = body
		} else {
			f.skip = body
		}
		pos += head
	}
	f.buf = append(f.buf[:0], f.buf[pos:]...)
	if len(f.buf) > maxRFBBuffer {
		return nil, errors.New("rfb: server message header too large")
	}
	return out, nil
}

// next sizes the header at the head of b and the body that follows it, and says whether both go to the viewer; head 0
// waits for more bytes.
func (f *screenOnly) next(b []byte) (head, body int, keep bool, err error) {
	switch f.stage {
	case stageVersion:
		if len(b) < rfbVersionLen {
			return 0, 0, false, nil
		}
		f.stage = stageSecurity
		return rfbVersionLen, 0, true, nil
	case stageSecurity:
		return f.security(b)
	case stageResult:
		if len(b) < 4 {
			return 0, 0, false, nil
		}
		if binary.BigEndian.Uint32(b) == 0 {
			f.stage = stageInit
			return 4, 0, true, nil
		}
		return f.failure(b, 4)
	case stageInit:
		if len(b) < rfbServerInit {
			return 0, 0, false, nil
		}
		if b[4] != rfbBitsPerPixel {
			return 0, 0, false, fmt.Errorf("rfb: server pixel format of %d bits; the bridge frames only %d", b[4], rfbBitsPerPixel)
		}
		f.stage = stageMessages
		return rfbServerInit, int(binary.BigEndian.Uint32(b[20:24])), true, nil
	case stageFailed:
		return 0, 0, false, errors.New("rfb: data after the server refused the session")
	}
	if f.rects > 0 {
		return f.rect(b)
	}
	return f.message(b)
}

func (f *screenOnly) security(b []byte) (head, body int, keep bool, err error) {
	switch f.session.minor.Load() {
	case 0:
		return 0, 0, false, errors.New("rfb: server security before the viewer's version")
	case 3:
		if len(b) < 4 {
			return 0, 0, false, nil
		}
		switch binary.BigEndian.Uint32(b) {
		case rfbSecurityNone:
			f.stage = stageInit
			return 4, 0, true, nil
		case 0:
			return f.failure(b, 4)
		default:
			return 0, 0, false, errors.New("rfb: server chose a security type other than None")
		}
	}
	if n := int(b[0]); n > 0 {
		if len(b) < 1+n {
			return 0, 0, false, nil
		}
		f.stage = stageInit
		if f.session.minor.Load() == 8 {
			f.stage = stageResult
		}
		return 1 + n, 0, true, nil
	}
	return f.failure(b, 1)
}

// failure frames the reason string that follows a refusal at offset at, after which the server closes.
func (f *screenOnly) failure(b []byte, at int) (head, body int, keep bool, err error) {
	if len(b) < at+4 {
		return 0, 0, false, nil
	}
	f.stage = stageFailed
	return at + 4, int(binary.BigEndian.Uint32(b[at : at+4])), true, nil
}

func (f *screenOnly) message(b []byte) (head, body int, keep bool, err error) {
	switch b[0] {
	case 0:
		if len(b) < 4 {
			return 0, 0, false, nil
		}
		f.rects = int(binary.BigEndian.Uint16(b[2:4]))
		return 4, 0, true, nil
	case 1:
		if len(b) < 6 {
			return 0, 0, false, nil
		}
		return 6, 6 * int(binary.BigEndian.Uint16(b[4:6])), true, nil
	case 2, 150:
		return 1, 0, true, nil
	case 3:
		if len(b) < 8 {
			return 0, 0, false, nil
		}
		n, err := cutTextLen(b)
		return 8, n - 8, false, err
	case 248:
		if len(b) < 9 {
			return 0, 0, false, nil
		}
		if b[8] > maxFencePayload {
			return 0, 0, false, errors.New("rfb: fence payload over 64 bytes")
		}
		return 9, int(b[8]), true, nil
	default:
		return 0, 0, false, fmt.Errorf("rfb: unsupported server message %d", b[0])
	}
}

func (f *screenOnly) rect(b []byte) (head, body int, keep bool, err error) {
	if len(b) < 12 {
		return 0, 0, false, nil
	}
	w, h := int(binary.BigEndian.Uint16(b[4:6])), int(binary.BigEndian.Uint16(b[6:8]))
	encoding := int32(binary.BigEndian.Uint32(b[8:12])) //nolint:gosec // encodings are signed
	if (encoding == encodingCopyRect || encoding == encodingZRLE || encoding == encodingExtendedSize) && len(b) < 16 {
		return 0, 0, false, nil
	}
	f.rects--
	switch encoding {
	case encodingRaw:
		return 12, w * h * rfbPixelBytes, true, nil
	case encodingCopyRect:
		return 16, 0, true, nil
	case encodingZRLE:
		return 16, int(binary.BigEndian.Uint32(b[12:16])), true, nil
	case encodingExtendedSize:
		return 16, 16 * int(b[12]), true, nil
	case encodingCursor:
		return 12, w*h*rfbPixelBytes + (w+7)/8*h, true, nil
	case encodingDesktopSize:
		return 12, 0, true, nil
	case encodingLastRect:
		f.rects = 0
		return 12, 0, true, nil
	default:
		return 0, 0, false, fmt.Errorf("rfb: unsupported encoding %d", encoding)
	}
}

// cutTextLen sizes a ClientCutText or ServerCutText at the head of b; the extended clipboard negates the length.
func cutTextLen(b []byte) (int, error) {
	length := int32(binary.BigEndian.Uint32(b[4:8])) //nolint:gosec // the extended clipboard sends its length negated
	if length == math.MinInt32 {
		return 0, errors.New("rfb: bad clipboard length")
	}
	return 8 + int(max(length, -length)), nil
}
