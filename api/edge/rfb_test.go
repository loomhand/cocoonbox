package edge

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strings"
	"testing"
)

func TestViewOnlyKeepsFramebufferMessagesAndDropsInput(t *testing.T) {
	handshake := []byte("RFB 003.008\n\x01\x00")
	kept := [][]byte{
		append([]byte{0, 0, 0, 0, 32}, make([]byte, 15)...),
		{2, 0, 0, 1, 0xff, 0xff, 0xfe, 0xc8},
		{3, 1, 0, 0, 0, 0, 5, 0, 3, 32},
		{150, 1, 0, 0, 0, 0, 5, 0, 3, 32},
		{248, 0, 0, 0, 0, 0, 0, 1, 2, 'o', 'k'},
	}
	dropped := [][]byte{
		{4, 1, 0, 0, 0, 0, 0, 0x61},
		{5, 1, 0, 10, 0, 20},
		append([]byte{6, 0, 0, 0, 0, 0, 0, 5}, "hello"...),
		append([]byte{6, 0, 0, 0, 0xff, 0xff, 0xff, 0xfc}, "caps"...),
		append([]byte{251, 0, 5, 0, 3, 32, 1, 0}, make([]byte, 16)...),
		{250, 0, 1, 2},
		{255, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 0x1e},
		{255, 1, 0, 2, 0, 0, 0, 0, 0, 2},
		{255, 1, 0, 0},
	}
	var in, want []byte
	in = append(in, handshake...)
	want = append(want, "RFB 003.008\n\x01\x01"...)
	for i := range max(len(kept), len(dropped)) {
		if i < len(dropped) {
			in = append(in, dropped[i]...)
		}
		if i < len(kept) {
			in = append(in, kept[i]...)
			want = append(want, kept[i]...)
		}
	}
	in = append(in, 2, 0, 0, 4, 0, 0, 0, 7, 0, 0, 0, 16, 0xc0, 0xa1, 0xe5, 0xce, 0xff, 0xff, 0xff, 0x11)
	want = append(want, 2, 0, 0, 2, 0, 0, 0, 16, 0xff, 0xff, 0xff, 0x11)
	for _, chunk := range []int{1, 3, 7, len(in)} {
		f := viewOnly{session: &rfbSession{}}
		var got []byte
		for rest := in; len(rest) > 0; {
			n := min(chunk, len(rest))
			out, err := f.feed(rest[:n])
			if err != nil {
				t.Fatalf("chunk %d: %v", chunk, err)
			}
			got = append(got, out...)
			rest = rest[n:]
		}
		if !bytes.Equal(got, want) {
			t.Errorf("chunk %d: forwarded %v, want %v", chunk, got, want)
		}
	}
}

func TestViewOnlySkipsALongClipboardWithoutBufferingIt(t *testing.T) {
	f := viewOnly{session: &rfbSession{}}
	if _, err := f.feed([]byte("RFB 003.003\n\x00")); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if out, err := f.feed([]byte{6, 0, 0, 0, 0x00, 0x40, 0x00, 0x00}); err != nil || len(out) != 0 {
		t.Fatalf("clipboard header: %v %v", out, err)
	}
	text := []byte(strings.Repeat("x", 1<<16))
	for range 64 {
		if out, err := f.feed(text); err != nil || len(out) != 0 || len(f.buf) != 0 {
			t.Fatalf("clipboard text: forwarded %d, buffered %d, %v", len(out), len(f.buf), err)
		}
	}
	if out, err := f.feed([]byte{3, 0, 0, 0, 0, 0, 5, 0, 3, 32}); err != nil || len(out) != 10 {
		t.Errorf("request after the clipboard: %v %v", out, err)
	}
}

func TestViewOnlyEndsOnAnythingElse(t *testing.T) {
	for name, in := range map[string][]byte{
		"unknown version":     []byte("RFB 004.000\n"),
		"vnc auth":            []byte("RFB 003.008\n\x02"),
		"unknown message":     []byte("RFB 003.008\n\x01\x01\x07"),
		"long fence":          append([]byte("RFB 003.008\n\x01\x01"), 248, 0, 0, 0, 0, 0, 0, 0, 65),
		"unknown qemu":        append([]byte("RFB 003.008\n\x01\x01"), 255, 9, 0, 0),
		"min int32 clipboard": append([]byte("RFB 003.008\n\x01\x01"), 6, 0, 0, 0, 0x80, 0, 0, 0),
		"16-bit pixels":       append([]byte("RFB 003.008\n\x01\x01"), append([]byte{0, 0, 0, 0, 16}, make([]byte, 15)...)...),
	} {
		f := viewOnly{session: &rfbSession{}}
		if out, err := f.feed(in); err == nil {
			t.Errorf("%s: forwarded %v without an error", name, out)
		}
	}
}

func TestScreenOnlyDropsTheClipboardAndKeepsFramesWhole(t *testing.T) {
	stream, want := serverStream()
	for _, chunk := range []int{1, 5, 7, 64, len(stream)} {
		session := &rfbSession{}
		session.minor.Store(8)
		f := screenOnly{session: session}
		var got []byte
		for rest := stream; len(rest) > 0; {
			n := min(chunk, len(rest))
			out, err := f.feed(rest[:n])
			if err != nil {
				t.Fatalf("chunk %d: %v", chunk, err)
			}
			got = append(got, out...)
			rest = rest[n:]
		}
		if !bytes.Equal(got, want) {
			t.Errorf("chunk %d: forwarded %d bytes %v, want %d bytes %v", chunk, len(got), got, len(want), want)
		}
		if bytes.Contains(got, []byte("secret")) || bytes.Contains(got, []byte("password")) {
			t.Errorf("chunk %d: the clipboard reached the viewer", chunk)
		}
	}
}

func TestScreenOnlyEndsOnAnythingElse(t *testing.T) {
	head, _ := serverStream()
	head = head[:12+2+4+24+4]
	for name, in := range map[string][]byte{
		"unknown message":    append(slices.Clone(head), 9),
		"tight rect":         append(slices.Clone(head), 0, 0, 0, 1, 0, 0, 0, 0, 0, 1, 0, 1, 0, 0, 0, 7),
		"extended clipboard": append(slices.Clone(head), 0, 0, 0, 1, 0, 0, 0, 0, 0, 1, 0, 1, 0xc0, 0xa1, 0xe5, 0xce),
	} {
		session := &rfbSession{}
		session.minor.Store(8)
		f := screenOnly{session: session}
		if _, err := f.feed(in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	f := screenOnly{session: &rfbSession{}}
	if _, err := f.feed([]byte("RFB 003.008\n\x01\x01")); err == nil {
		t.Error("security before the viewer chose a version: no error")
	}
}

// serverStream is a 3.8 handshake and a run of server messages with plain and extended clipboard text interleaved,
// and what of it a viewer must get.
func serverStream() (stream, want []byte) {
	be := binary.BigEndian
	pixelFormat := []byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0}
	keep := func(parts ...[]byte) {
		for _, p := range parts {
			stream = append(stream, p...)
			want = append(want, p...)
		}
	}
	drop := func(p []byte) { stream = append(stream, p...) }
	rect := func(w, h uint16, encoding int32, extra ...byte) []byte {
		r := be.AppendUint16(be.AppendUint16(be.AppendUint16(be.AppendUint16(nil, 0), 0), w), h)
		return append(be.AppendUint32(r, uint32(encoding)), extra...) //nolint:gosec // encodings are signed
	}
	cut := func(length int32, text string) []byte {
		return append(be.AppendUint32([]byte{3, 0, 0, 0}, uint32(length)), text...) //nolint:gosec // negated by the extended clipboard
	}
	keep([]byte("RFB 003.008\n"), []byte{1, 1}, []byte{0, 0, 0, 0})
	keep([]byte{5, 0, 3, 32}, pixelFormat, be.AppendUint32(nil, 4), []byte("test"))
	keep([]byte{0, 0, 0, 2}, rect(16, 16, encodingZRLE, 0, 0, 0, 5), []byte("zrle!"), rect(16, 16, encodingCopyRect, 0, 1, 0, 2))
	drop(cut(6, "secret"))
	keep([]byte{2})
	drop(cut(-12, "\x10\x00\x00\x00password"))
	keep([]byte{0, 0, 0xff, 0xff}, rect(2, 2, encodingRaw), make([]byte, 16), rect(9, 2, encodingCursor), make([]byte, 9*2*4+2*2))
	keep(rect(0, 0, encodingExtendedSize, 1, 0, 0, 0), make([]byte, 16), rect(0, 0, encodingLastRect))
	keep([]byte{1, 0, 0, 0, 0, 2}, make([]byte, 12))
	keep([]byte{248, 0, 0, 0, 0, 0, 0, 4, 2}, []byte("ab"), []byte{150})
	return stream, want
}
