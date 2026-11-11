package tuigateway

import (
	"bytes"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Transport stand in that records the frames written to it
type recordingTransport struct{ frames []*pyjson.Object }

func (r *recordingTransport) Write(obj *pyjson.Object) bool {
	r.frames = append(r.frames, obj)
	return true
}

func (r *recordingTransport) Close() {}

// Redirects the real stdout of the server to a buffer for one test
func captureStdout(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	saved := realStdout
	realStdout = buf
	savedChannel := StdioIsRPCChannel()
	t.Cleanup(func() {
		realStdout = saved
		SetStdioIsRPCChannel(savedChannel)
	})
	return buf
}

func TestUnregisterLiveTransportStopsDelivery(t *testing.T) {
	buf := captureStdout(t)
	SetStdioIsRPCChannel(true)
	a := &recordingTransport{}
	RegisterLiveTransport(a)
	UnregisterLiveTransport(a)

	BroadcastGlobalEvent("skin.changed", map[string]any{"name": "x"})

	if len(a.frames) != 0 {
		t.Fatalf("frames = %v", a.frames)
	}
	frame, err := pyjson.LoadsDict(buf.String())
	if err != nil {
		t.Fatalf("stdout %q: %v", buf.String(), err)
	}
	params, _ := frame.GetOr("params", nil).(*pyjson.Object)
	if params == nil || params.GetOr("type", nil) != "skin.changed" {
		t.Fatalf("frame = %q", buf.String())
	}
}

func TestPeerlessGlobalBroadcastNeverReachesStdoutInWsBackend(t *testing.T) {
	buf := captureStdout(t)
	SetStdioIsRPCChannel(false)
	a := &recordingTransport{}
	RegisterLiveTransport(a)
	BroadcastGlobalEvent("sessions.changed", map[string]any{})
	UnregisterLiveTransport(a)

	BroadcastGlobalEvent("sessions.changed", map[string]any{})

	if len(a.frames) != 1 {
		t.Fatalf("frames = %d", len(a.frames))
	}
	if buf.String() != "" {
		t.Fatalf("stdout = %q", buf.String())
	}
}
