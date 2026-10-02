package edge

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	stageVersion = iota
	stageSecurity
	stageInit
	stageMessages

	rfbVersionLen   = 12
	rfbSecurityNone = 1
	rfbShared       = 1
	maxFencePayload = 64
	maxViewerBuffer = 1 << 20
)

var (
	rfbVersions = map[string]bool{"RFB 003.003\n": true, "RFB 003.007\n": true, "RFB 003.008\n": true}

	// rfbFixedLen sizes the client messages of a fixed length: the framebuffer ones a viewer keeps, then the input it loses.
	rfbFixedLen = map[byte]int{0: 20, 3: 10, 150: 10, 4: 8, 5: 6, 250: 4}
	// rfbHeaderLen is how much of a variable-length message names its length.
	rfbHeaderLen = map[byte]int{2: 4, 6: 8, 248: 9, 251: 8, 255: 4}
	rfbKept      = map[byte]bool{0: true, 2: true, 3: true, 150: true, 248: true}
)

// viewOnly filters the client side of an RFB stream: it passes the handshake, pixel format, encodings and framebuffer
// requests, forces a shared session and drops key, pointer, clipboard, resize, xvp and QEMU messages; anything else ends the stream.
type viewOnly struct {
	stage int
	v33   bool
	buf   []byte
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
		size, keep, err := f.next()
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
		if keep {
			out = append(out, f.buf[:size]...)
		}
		f.buf = f.buf[size:]
	}
	if len(f.buf) > maxViewerBuffer {
		return nil, errors.New("rfb: viewer message too large")
	}
	f.buf = append([]byte(nil), f.buf...)
	return out, nil
}

// next sizes the unit at the head of buf and says whether it is forwarded; size 0 waits for more bytes, and a size past
// the buffer is only ever returned for a dropped message.
func (f *viewOnly) next() (int, bool, error) {
	b := f.buf
	switch f.stage {
	case stageVersion:
		if len(b) < rfbVersionLen {
			return 0, false, nil
		}
		version := string(b[:rfbVersionLen])
		if !rfbVersions[version] {
			return 0, false, fmt.Errorf("rfb: unsupported version %q", version)
		}
		f.v33 = version == "RFB 003.003\n"
		f.stage = stageSecurity
		if f.v33 {
			f.stage = stageInit
		}
		return rfbVersionLen, true, nil
	case stageSecurity:
		if b[0] != rfbSecurityNone {
			return 0, false, fmt.Errorf("rfb: security type %d is not None", b[0])
		}
		f.stage = stageInit
		return 1, true, nil
	case stageInit:
		b[0] = rfbShared
		f.stage = stageMessages
		return 1, true, nil
	}
	size, err := messageLen(b)
	keep := rfbKept[b[0]]
	if keep && size > len(b) {
		return 0, false, err
	}
	return size, keep, err
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
		length := int32(binary.BigEndian.Uint32(b[4:8])) //nolint:gosec // the extended clipboard sends its length negated
		if length == math.MinInt32 {
			return 0, errors.New("rfb: bad clipboard length")
		}
		return 8 + int(max(length, -length)), nil
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
