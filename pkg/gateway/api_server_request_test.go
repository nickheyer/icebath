package gateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Historical inline SSE encoder, an event line then the ASCII JSON data line
func inlineSSEFrame(t *testing.T, data any, event string) []byte {
	encoded, err := jsonutil.Encode(data, jsonutil.ASCII(), jsonutil.Spaced())
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
		data  *jsonutil.Object
	}{{"icebath.tool.progress", jsonutil.ObjectOf("name", "x", "status", "running")},
		{"response.created", jsonutil.ObjectOf("id", "r1", "status", "in_progress")}} {
		if got := mustSSEFrame(t, c.data, c.event, true); string(got) != string(inlineSSEFrame(t, c.data, c.event)) {
			t.Fatalf("%s: %q", c.event, got)
		}
	}
}

func TestSSEFrameDefaultEnsureASCIIMatchesBareJSON(t *testing.T) {
	payload := jsonutil.ObjectOf("text", "café — Münchner \U0001F3D4")
	got := mustSSEFrame(t, payload, "", true)
	if string(got) != string(inlineSSEFrame(t, payload, "")) || !strings.Contains(string(got), "\\u00e9") {
		t.Fatalf("%q", got)
	}
}

func TestSSEFrameEnsureASCIIFalseReproducesSessionEventStream(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload *jsonutil.Object
	}{{"session.update", jsonutil.ObjectOf("text", "café — Münchner \U0001F3D4", "id", int64(1))},
		{"thread.message.delta", jsonutil.ObjectOf("content", "héllo wörld ✓", "seq", int64(3))}} {
		data, _ := jsonutil.Encode(c.payload, jsonutil.Spaced())
		want := "event: " + c.name + "\ndata: " + data + "\n\n"
		if got := mustSSEFrame(t, c.payload, c.name, false); string(got) != want {
			t.Fatalf("%q", got)
		}
	}
}

func TestSSEFrameTypedObjectRoundtrip(t *testing.T) {
	obj := jsonutil.ObjectOf("id", "x", "choices", []any{jsonutil.ObjectOf("index", int64(0), "delta", jsonutil.ObjectOf("content", "hi"))})
	out := string(mustSSEFrame(t, obj, "", true))
	line := strings.TrimSpace(strings.SplitN(out, "data: ", 2)[1])
	var decoded, want any
	json.Unmarshal([]byte(line), &decoded)
	wantText, _ := jsonutil.Encode(obj, jsonutil.Spaced(), jsonutil.ASCII())
	json.Unmarshal([]byte(wantText), &want)
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("%v", decoded)
	}
}

func TestEveryConfigurableLevelIsAccepted(t *testing.T) {
	for _, level := range config.ValidReasoningEfforts {
		got := apiRequestReasoningConfig(jsonutil.ObjectOf("reasoning_effort", level))
		if !reflect.DeepEqual(got, map[string]any{"enabled": true, "effort": level}) {
			t.Fatalf("%s: %v", level, got)
		}
	}
}

func TestNoneDisables(t *testing.T) {
	if got := apiRequestReasoningConfig(jsonutil.ObjectOf("reasoning_effort", "none")); !reflect.DeepEqual(got, map[string]any{"enabled": false}) {
		t.Fatalf("%v", got)
	}
}

func TestStructuredObjectShape(t *testing.T) {
	got := apiRequestReasoningConfig(jsonutil.ObjectOf("reasoning", jsonutil.ObjectOf("enabled", true, "effort", "ultra")))
	if !reflect.DeepEqual(got, map[string]any{"enabled": true, "effort": "ultra"}) {
		t.Fatalf("%v", got)
	}
}

func TestUnknownLevelStillIgnored(t *testing.T) {
	if got := apiRequestReasoningConfig(jsonutil.ObjectOf("reasoning_effort", "turbo")); got != nil {
		t.Fatalf("%v", got)
	}
}
