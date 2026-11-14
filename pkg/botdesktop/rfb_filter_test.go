package botdesktop

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
)

var rfbHandshake = []byte("RFB 003.008\n\x01\x01")

func rfbClipboardHeader(length int64) []byte {
	out := []byte{6, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[4:], uint32(int32(length)))
	return out
}

func TestOversizedClipboardIsRejectedAtHeaderWithoutWaitingForPayload(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, length := range []int64{rfbMaxCutText + 1, -rfbMaxCutText - 1, 1<<31 - 1, -(1 << 31)} {
		for _, holder := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d-%v", length, holder), func(t *testing.T) {
				parser := NewRfbClientFilter(func() bool { return holder })
				if _, err := parser.Feed(rfbHandshake); err != nil {
					t.Fatal(err)
				}
				header := rfbClipboardHeader(length)
				for _, b := range header[:len(header)-1] {
					if out, err := parser.Feed([]byte{b}); err != nil || len(out) != 0 {
						t.Fatalf("partial header gave %q %v", out, err)
					}
				}
				_, err := parser.Feed(header[len(header)-1:])
				var valueErr *RfbFilterError
				if !errors.As(err, &valueErr) || !strings.Contains(err.Error(), "clipboard") {
					t.Fatalf("err %v, want clipboard ValueError", err)
				}
			})
		}
	}
}

func rfbDesktopSizeMessage(width, height uint16, screens int) []byte {
	head := []byte{251, 0, 0, 0, 0, 0, byte(screens), 0}
	binary.BigEndian.PutUint16(head[2:], width)
	binary.BigEndian.PutUint16(head[4:], height)
	for i := range screens {
		screen := make([]byte, 16)
		binary.BigEndian.PutUint32(screen[0:], uint32(i))
		binary.BigEndian.PutUint16(screen[8:], width)
		binary.BigEndian.PutUint16(screen[10:], height)
		head = append(head, screen...)
	}
	return head
}

func TestSetDesktopSizeIsFramedAndGatedLikeInput(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, holder := range []bool{false, true} {
		parser := NewRfbClientFilter(func() bool { return holder })
		if _, err := parser.Feed(rfbHandshake); err != nil {
			t.Fatal(err)
		}
		resize := rfbDesktopSizeMessage(1280, 800, 1)
		updateRequest := append([]byte{3, 0}, make([]byte, 8)...)
		out, err := parser.Feed(append(append([]byte(nil), resize...), updateRequest...))
		want := append([]byte(nil), updateRequest...)
		if holder {
			want = append(append([]byte(nil), resize...), updateRequest...)
		}
		if err != nil || !bytes.Equal(out, want) {
			t.Fatalf("holder %v: out %v err %v", holder, out, err)
		}
	}
}

func TestTruncatedSetDesktopSizeWaitsForTheRest(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	parser := NewRfbClientFilter(func() bool { return true })
	if _, err := parser.Feed(rfbHandshake); err != nil {
		t.Fatal(err)
	}
	msg := rfbDesktopSizeMessage(1280, 800, 2)
	for _, b := range msg[:len(msg)-1] {
		if out, err := parser.Feed([]byte{b}); err != nil || len(out) != 0 {
			t.Fatalf("partial message gave %v %v", out, err)
		}
	}
	if out, err := parser.Feed(msg[len(msg)-1:]); err != nil || !bytes.Equal(out, msg) {
		t.Fatalf("out %v err %v", out, err)
	}
}

func TestRfbFilterForwardsInputOnlyFromTheLeaseHolderAcrossArbitraryChunking(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	key := []byte("\x04\x01\x00\x00\x00\x00\x00\x61")
	qemuKey := []byte{255, 0, 0, 1, 0, 0, 0, 0x65, 0, 0, 0, 0x12}
	pointer := []byte("\x05\x01\x00\x10\x00\x10")
	cut := []byte("\x06\x00\x00\x00\x00\x00\x00\x02hi")
	fbur := append([]byte{3, 0}, make([]byte, 8)...)
	setenc := []byte("\x02\x00\x00\x02\x00\x00\x00\x07\xff\xff\xff\x21")
	f := NewRfbClientFilter(func() bool { return LeaseViewerMaySendInput("v1", "") })
	head, err := f.Feed(rfbHandshake)
	if err != nil || head[len(head)-1] != 0x01 {
		t.Fatalf("ClientInit is forced shared so a viewer never kicks the agent's watcher: %v %v", head, err)
	}
	stream := bytes.Join([][]byte{key, fbur, pointer, setenc, cut, qemuKey}, nil)
	var out []byte
	for i := range stream {
		chunk, err := f.Feed(stream[i : i+1])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, chunk...)
	}
	if !bytes.Equal(out, append(append([]byte(nil), fbur...), setenc...)) {
		t.Fatalf("agent holds: out %v", out)
	}
	if _, err := LeaseAcquire("v1", "", ""); err != nil {
		t.Fatal(err)
	}
	input := bytes.Join([][]byte{key, pointer, qemuKey}, nil)
	if got, err := f.Feed(input); err != nil || !bytes.Equal(got, input) {
		t.Fatalf("holder input %v %v", got, err)
	}
	if _, err := LeaseAcquire("v2", "", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := f.Feed(key); err != nil || len(got) != 0 {
		t.Fatalf("evicted viewer input %v %v", got, err)
	}
	v1, v2 := "v1", "v2"
	if l, err := LeaseRelease(LeaseReleaseOptions{ViewerID: &v1}); err != nil || l.Holder != LeaseHuman {
		t.Fatalf("a stale viewer's release must not yank control from v2: %+v %v", l, err)
	}
	if l, err := LeaseRelease(LeaseReleaseOptions{ViewerID: &v2}); err != nil || l.Holder != LeaseAgent {
		t.Fatalf("v2 release %+v %v", l, err)
	}
}
