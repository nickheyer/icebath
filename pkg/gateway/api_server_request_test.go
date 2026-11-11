package gateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Historical inline SSE encoder, an event line then the ASCII JSON data line
func inlineSSEFrame(t *testing.T, data any, event string) []byte {
	encoded, err := pyjson.Dumps(data)
	if err != nil {
		t.Fatal(err)
	}
	prefix := ""
	if event != "" {
		prefix = "event: " + event + "\n"
	}
	return []byte(prefix + "data: " + encoded + "\n\n")
}

func mustSSEFrame(t *testing.T, data any, event string, ensureASCII bool) []byte {
	frame, err := apiSSEFrame(data, event, ensureASCII, nil)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestSSEFrameMatchesInlineEncoderWithEvent(t *testing.T) {
	for _, c := range []struct {
		event string
		data  *pyjson.Object
	}{{"hermes.tool.progress", pyjson.ObjectOf("name", "x", "status", "running")},
		{"response.created", pyjson.ObjectOf("id", "r1", "status", "in_progress")}} {
		if got := mustSSEFrame(t, c.data, c.event, true); string(got) != string(inlineSSEFrame(t, c.data, c.event)) {
			t.Fatalf("%s: %q", c.event, got)
		}
	}
}

func TestSSEFrameDefaultEnsureASCIIMatchesBareJSON(t *testing.T) {
	payload := pyjson.ObjectOf("text", "café — Münchner \U0001F3D4")
	got := mustSSEFrame(t, payload, "", true)
	if string(got) != string(inlineSSEFrame(t, payload, "")) || !strings.Contains(string(got), "\\u00e9") {
		t.Fatalf("%q", got)
	}
}

func TestSSEFrameEnsureASCIIFalseReproducesSessionEventStream(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload *pyjson.Object
	}{{"session.update", pyjson.ObjectOf("text", "café — Münchner \U0001F3D4", "id", int64(1))},
		{"thread.message.delta", pyjson.ObjectOf("content", "héllo wörld ✓", "seq", int64(3))}} {
		data, _ := pyjson.Dumps(c.payload, pyjson.EnsureASCII(false))
		want := "event: " + c.name + "\ndata: " + data + "\n\n"
		if got := mustSSEFrame(t, c.payload, c.name, false); string(got) != want {
			t.Fatalf("%q", got)
		}
	}
}

func TestSSEFrameTypedObjectRoundtrip(t *testing.T) {
	obj := pyjson.ObjectOf("id", "x", "choices", []any{pyjson.ObjectOf("index", int64(0), "delta", pyjson.ObjectOf("content", "hi"))})
	out := string(mustSSEFrame(t, obj, "", true))
	line := strings.TrimSpace(strings.SplitN(out, "data: ", 2)[1])
	var decoded, want any
	json.Unmarshal([]byte(line), &decoded)
	wantText, _ := pyjson.Dumps(obj)
	json.Unmarshal([]byte(wantText), &want)
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("%v", decoded)
	}
}

func TestEveryConfigurableLevelIsAccepted(t *testing.T) {
	for _, level := range config.ValidReasoningEfforts {
		got := apiRequestReasoningConfig(pyjson.ObjectOf("reasoning_effort", level))
		if !reflect.DeepEqual(got, map[string]any{"enabled": true, "effort": level}) {
			t.Fatalf("%s: %v", level, got)
		}
	}
}

func TestNoneDisables(t *testing.T) {
	if got := apiRequestReasoningConfig(pyjson.ObjectOf("reasoning_effort", "none")); !reflect.DeepEqual(got, map[string]any{"enabled": false}) {
		t.Fatalf("%v", got)
	}
}

func TestStructuredObjectShape(t *testing.T) {
	got := apiRequestReasoningConfig(pyjson.ObjectOf("reasoning", pyjson.ObjectOf("enabled", true, "effort", "ultra")))
	if !reflect.DeepEqual(got, map[string]any{"enabled": true, "effort": "ultra"}) {
		t.Fatalf("%v", got)
	}
}

func TestUnknownLevelStillIgnored(t *testing.T) {
	if got := apiRequestReasoningConfig(pyjson.ObjectOf("reasoning_effort", "turbo")); got != nil {
		t.Fatalf("%v", got)
	}
}
