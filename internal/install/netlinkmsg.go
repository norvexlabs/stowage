package install

import (
	"encoding/binary"
	"fmt"
)

// netlinkMsg is one message of a netlink answer: its type, sequence and payload.
type netlinkMsg struct {
	typ  uint16
	seq  uint32
	data []byte
}

// parseNetlink splits a netlink answer into its messages, each 4-byte aligned.
func parseNetlink(b []byte) ([]netlinkMsg, error) {
	const hdr = 16
	var out []netlinkMsg
	for len(b) >= hdr {
		l := int(binary.NativeEndian.Uint32(b))
		if l < hdr || l > len(b) {
			return nil, fmt.Errorf("a netlink message of %d bytes in %d", l, len(b))
		}
		out = append(out, netlinkMsg{typ: binary.NativeEndian.Uint16(b[4:]), seq: binary.NativeEndian.Uint32(b[8:]), data: b[hdr:l]})
		b = b[min((l+3)&^3, len(b)):]
	}
	return out, nil
}
