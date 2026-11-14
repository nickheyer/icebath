package install

import (
	"encoding/json"
	"testing"
)

func TestStripAnsiLeavesOnlyTheTextATerminalWouldShow(t *testing.T) {
	cases := [][2]string{
		{"\x1b[0;32m✓\x1b[0m Detected: macos (macos)", "✓ Detected: macos (macos)"},
		{"\x1b[2K\x1b[1GCloning repository…", "Cloning repository…"},
		{"down\x1b[?25lloading\x1b[K", "downloading"},
		{"\x1b]0;icebath\x07Installing Icebath", "Installing Icebath"},
		{"\x1b]2;icebath\x1b\\Installing Icebath", "Installing Icebath"},
		{"\r 12%\r 67%\r100%", "100%"},
		{"Resolving dependencies…\r", "Resolving dependencies…"},
		{"\r\r", ""},
		{"ok\x1b[0;3", "ok"},
		{"ok\x1b", "ok"},
		{"Ready — café ✓ 中文", "Ready — café ✓ 中文"},
		{"", ""},
	}
	for _, c := range cases {
		if got := StripANSI(c[0]); got != c[1] {
			t.Fatalf("input %q: got %q want %q", c[0], got, c[1])
		}
	}
}

func TestSanitizedForUIOnlyTouchesLogLines(t *testing.T) {
	if got := LogEvent("", "\x1b[1;32mdone\x1b[0m\r", Stdout).SanitizedForUI().Line; got != "done" {
		t.Fatalf("log line %q", got)
	}
	failed := FailedEvent("", "\x1b[0;31mfatal\x1b[0m").SanitizedForUI()
	if *failed.Error != "\x1b[0;31mfatal\x1b[0m" {
		t.Fatalf("failed error rewritten: %q", *failed.Error)
	}
}

// Every payload carries exactly the keys of the UI contract
func TestEventPayloadShapesMatchTheUIContract(t *testing.T) {
	version := 1
	result := StageResult{Stage: "config", OK: true}
	cases := []struct {
		event Event
		want  string
	}{
		{ManifestEvent([]StageInfo{{Name: "setup", Title: "T", Category: "configuration", NeedsUserInput: true}}, &version),
			`{"type":"manifest","stages":[{"name":"setup","title":"T","category":"configuration","needs_user_input":true}],"protocolVersion":1}`},
		{ManifestEvent(nil, nil), `{"type":"manifest","stages":[],"protocolVersion":null}`},
		{StageEvent("config", StateRunning, nil, nil, nil), `{"type":"stage","name":"config","state":"running"}`},
		{StageEvent("config", StateSucceeded, int64Ptr(12), &result, nil),
			`{"type":"stage","name":"config","state":"succeeded","durationMs":12,"result":{"stage":"config","ok":true,"skipped":false,"reason":null,"data":null}}`},
		{StageEvent("x", StateFailed, int64Ptr(0), nil, strPtr("boom")), `{"type":"stage","name":"x","state":"failed","durationMs":0,"error":"boom"}`},
		{LogEvent("", "hi", Stderr), `{"type":"log","line":"hi","stream":"stderr"}`},
		{LogEvent("config", "hi", Stdout), `{"type":"log","stage":"config","line":"hi","stream":"stdout"}`},
		{CompleteEvent("/r", nil), `{"type":"complete","installRoot":"/r","marker":null}`},
		{CompleteEvent("/r", json.RawMessage(`{"a":1}`)), `{"type":"complete","installRoot":"/r","marker":{"a":1}}`},
		{FailedEvent("", "e"), `{"type":"failed","error":"e"}`},
		{FailedEvent("update", "e"), `{"type":"failed","stage":"update","error":"e"}`},
	}
	for _, c := range cases {
		data, err := json.Marshal(c.event)
		if err != nil || string(data) != c.want {
			t.Fatalf("got %s (%v) want %s", data, err, c.want)
		}
	}
}
