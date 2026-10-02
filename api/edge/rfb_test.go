package edge

import (
	"bytes"
	"strings"
	"testing"
)

func TestViewOnlyKeepsFramebufferMessagesAndDropsInput(t *testing.T) {
	handshake := []byte("RFB 003.008\n\x01\x00")
	kept := [][]byte{
		append([]byte{0, 0, 0, 0}, make([]byte, 16)...),
		{2, 0, 0, 2, 0, 0, 0, 7, 0xff, 0xff, 0xfe, 0xc8},
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
	for _, chunk := range []int{1, 3, 7, len(in)} {
		var f viewOnly
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
	var f viewOnly
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
	} {
		var f viewOnly
		if out, err := f.feed(in); err == nil {
			t.Errorf("%s: forwarded %v without an error", name, out)
		}
	}
}
