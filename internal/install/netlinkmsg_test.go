package install

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func netlinkBytes(typ uint16, seq uint32, data []byte) []byte {
	b := make([]byte, 16+len(data), (16+len(data)+3)&^3)
	binary.NativeEndian.PutUint32(b, uint32(len(b)))
	binary.NativeEndian.PutUint16(b[4:], typ)
	binary.NativeEndian.PutUint32(b[8:], seq)
	copy(b[16:], data)
	return b[:cap(b)]
}

func TestParseNetlinkSplitsAnAnswer(t *testing.T) {
	t.Parallel()
	b := append(netlinkBytes(2, 1, []byte{0, 0, 0, 0}), netlinkBytes(3, 2, []byte{1})...)
	msgs, err := parseNetlink(b)
	if err != nil || len(msgs) != 2 || msgs[0].typ != 2 || msgs[0].seq != 1 || len(msgs[0].data) != 4 || msgs[1].seq != 2 || len(msgs[1].data) != 1 {
		t.Fatalf("%v %+v", err, msgs)
	}
	for _, bad := range [][]byte{
		binary.NativeEndian.AppendUint32(nil, 8),   // shorter than its header
		binary.NativeEndian.AppendUint32(nil, 100), // longer than the answer
	} {
		bad = append(bad, make([]byte, 12)...)
		if _, err := parseNetlink(bad); err == nil {
			t.Errorf("%x: no error", bad)
		}
	}
}

func FuzzParseNetlink(f *testing.F) {
	f.Add(netlinkBytes(2, 1, []byte{0, 0, 0, 0}))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		msgs, err := parseNetlink(b)
		if err != nil {
			return
		}
		// Each message is its header and its data, laid end to end, each but the last 4-byte
		// aligned, within the answer.
		at := 0
		for _, m := range msgs {
			l := 16 + len(m.data)
			if at+l > len(b) || binary.NativeEndian.Uint32(b[at:]) != uint32(l) || !bytes.Equal(m.data, b[at+16:at+l]) {
				t.Fatalf("a message at %d is not its header's %d bytes", at, l)
			}
			at += (l + 3) &^ 3
		}
		if at > len(b)+3 {
			t.Fatalf("messages of %d bytes in %d", at, len(b))
		}
	})
}
