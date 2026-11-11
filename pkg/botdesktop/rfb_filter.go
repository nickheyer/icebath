package botdesktop

import (
	"encoding/binary"
	"fmt"
)

var rfbInputTypes = map[byte]bool{4: true, 5: true, 6: true, 251: true, 255: true}

var rfbFixedLengths = map[byte]int{0: 20, 3: 10, 4: 8, 5: 6, 150: 10, 255: 12}

const (
	rfbSetEncodings   = 2
	rfbClientCutText  = 6
	rfbFence          = 248
	rfbSetDesktopSize = 251
	rfbMaxCutText     = 256 * 1024
)

// RFB message error of the client stream, a ValueError in the filter it ports
type RfbFilterError struct{ Msg string }

func (e *RfbFilterError) Error() string      { return e.Msg }
func (e *RfbFilterError) PyTypeName() string { return "ValueError" }

// Stateful client to server RFB parser forwarding input only while allowInput answers true
type RfbClientFilter struct {
	allowInput    func() bool
	buf           []byte
	handshakeLeft int
}

// Filter over a client stream whose input messages pass only while allowInput holds
func NewRfbClientFilter(allowInput func() bool) *RfbClientFilter {
	return &RfbClientFilter{allowInput: allowInput, handshakeLeft: 12 + 1 + 1}
}

// Bytes of the fed chunk allowed to reach Xvnc, the ClientInit forced shared and gated input dropped
func (f *RfbClientFilter) Feed(chunk []byte) ([]byte, error) {
	f.buf = append(f.buf, chunk...)
	var out []byte
	if f.handshakeLeft > 0 {
		take := min(f.handshakeLeft, len(f.buf))
		if take > 0 {
			head := append([]byte(nil), f.buf[:take]...)
			if f.handshakeLeft-take == 0 {
				head[len(head)-1] = 0x01
			}
			out = append(out, head...)
			f.buf = f.buf[take:]
			f.handshakeLeft -= take
		}
		if f.handshakeLeft > 0 {
			return out, nil
		}
	}
	for len(f.buf) > 0 {
		length, known, err := f.messageLength()
		if err != nil {
			return nil, err
		}
		if !known || len(f.buf) < length {
			break
		}
		msg := append([]byte(nil), f.buf[:length]...)
		f.buf = f.buf[length:]
		if rfbInputTypes[msg[0]] && !f.allowInput() {
			continue
		}
		out = append(out, msg...)
	}
	return out, nil
}

// Length of the buffered message, false until enough of its header arrived
func (f *RfbClientFilter) messageLength() (int, bool, error) {
	t := f.buf[0]
	if n, ok := rfbFixedLengths[t]; ok {
		return n, true, nil
	}
	switch t {
	case rfbSetEncodings:
		if len(f.buf) < 4 {
			return 0, false, nil
		}
		return 4 + 4*int(binary.BigEndian.Uint16(f.buf[2:4])), true, nil
	case rfbClientCutText:
		if len(f.buf) < 8 {
			return 0, false, nil
		}
		n := int64(int32(binary.BigEndian.Uint32(f.buf[4:8])))
		if n < 0 {
			n = -n
		}
		if n > rfbMaxCutText {
			return 0, false, &RfbFilterError{Msg: "clipboard message too large"}
		}
		return 8 + int(n), true, nil
	case rfbFence:
		if len(f.buf) < 9 {
			return 0, false, nil
		}
		return 9 + int(f.buf[8]), true, nil
	case rfbSetDesktopSize:
		if len(f.buf) < 8 {
			return 0, false, nil
		}
		return 8 + 16*int(f.buf[6]), true, nil
	}
	return 0, false, &RfbFilterError{Msg: fmt.Sprintf("unknown RFB client message type %d", t)}
}
