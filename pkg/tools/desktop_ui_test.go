package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Env over a temp home with the config text written to config.yaml
func desktopTestEnv(t *testing.T, configText string) *Env {
	t.Helper()
	h := t.TempDir()
	if configText != "" {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(configText), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Env{Home: h, Config: config.Defaults(), Values: &sync.Map{}}
}

// Captures emitted desktop events for assertions
type emitCapture struct {
	sid, event string
	payload    map[string]any
}

func captureEmitter(t *testing.T) *emitCapture {
	t.Helper()
	c := &emitCapture{}
	SetDesktopEmitter(func(_ *Env, sid, event string, payload map[string]any) error {
		c.sid, c.event, c.payload = sid, event, payload
		return nil
	})
	t.Cleanup(func() { SetDesktopEmitter(nil) })
	return c
}

func mustJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	return m
}

func TestDesktopEmitRoutesByWindowAndSessionScope(t *testing.T) {
	env := desktopTestEnv(t, "")
	c := captureEmitter(t)
	t.Setenv("ICEBATH_UI_SESSION_ID", "win-1")
	t.Setenv("ICEBATH_SESSION_ID", "chat-9")
	if ok, _ := desktopEmitEvent(env, "pane.reveal", map[string]any{"pane": "chat"}); !ok || c.sid != "win-1" {
		t.Fatalf("window event sid %q", c.sid)
	}
	if ok, _ := desktopEmitEvent(env, "message.reaction", nil); !ok || c.sid != "chat-9" {
		t.Fatalf("session scoped event sid %q", c.sid)
	}
	if ok, err := DesktopEmitEvent(env, "message.reaction", nil); !ok || err != nil || c.sid != "chat-9" {
		t.Fatalf("exported emit sid %q", c.sid)
	}
	SetDesktopEmitter(nil)
	if ok, _ := desktopEmitEvent(env, "pane.reveal", nil); ok || DesktopAvailable() {
		t.Fatal("no emitter must report unavailable")
	}
}

func TestDesktopUserEnabledReadsDisplaySwitches(t *testing.T) {
	env := desktopTestEnv(t, "display:\n  in_app_tips: false\n")
	if desktopUserEnabled(env, "in_app_tips", true) {
		t.Fatal("explicit false ignored")
	}
	if !desktopUserEnabled(env, "in_app_tours", true) {
		t.Fatal("absent switch must keep the default")
	}
	if DesktopUserEnabled(env, "in_app_tips", true) || !DesktopUserEnabled(env, "in_app_tours", true) {
		t.Fatal("exported switch read differs")
	}
	if !desktopUserEnabled(&Env{}, "in_app_tips", true) {
		t.Fatal("unreadable config must keep the default")
	}
}

func TestTipAndFocusPaneValidation(t *testing.T) {
	env := desktopTestEnv(t, "")
	if r := tipTool(env, "", "#a", "", ""); !strings.Contains(r, "tip needs text") {
		t.Fatal(r)
	}
	if r := tipTool(env, "hi", "", "", ""); !strings.Contains(r, "tip needs a selector") {
		t.Fatal(r)
	}
	if r := tipTool(env, "hi", "#a", "", "diagonal"); !strings.Contains(r, "side must be one of: top, right, bottom, left.") {
		t.Fatal(r)
	}
	if r := tipTool(env, "hi", "#a", "", ""); !strings.Contains(r, "only available in the Icebath desktop app") {
		t.Fatal(r)
	}
	c := captureEmitter(t)
	r := tipTool(env, " hi ", " #a ", "T", "left")
	if mustJSON(t, r)["success"] != true || c.event != "tip.show" || c.payload["title"] != "T" || c.payload["side"] != "left" || c.payload["text"] != "hi" {
		t.Fatalf("%s %v", r, c.payload)
	}
	if r := focusPaneTool(env, "Nope"); !strings.Contains(r, "pane must be one of: chat, files, terminal, review, sessions.") {
		t.Fatal(r)
	}
	if r := focusPaneTool(env, " Files "); mustJSON(t, r)["pane"] != "files" || c.payload["pane"] != "files" {
		t.Fatal(r)
	}
}

func TestPreviewTargetNormalization(t *testing.T) {
	cases := map[string]string{
		"www.cnn.com":     "https://www.cnn.com",
		"localhost:3000":  "http://localhost:3000",
		"`example.org/x`": "https://example.org/x",
		"https://a.b/c":   "https://a.b/c",
		"/tmp/x.html":     "/tmp/x.html",
		"./rel.html":      "./rel.html",
		"~/doc.html":      "~/doc.html",
		"file:///tmp/a":   "file:///tmp/a",
		"[::1]:8080/":     "http://[::1]:8080/",
		"just words":      "just words",
		"":                "",
	}
	for in, want := range cases {
		if got := normalizePreviewTarget(in); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}

func TestOpenPreviewRefusesDirectories(t *testing.T) {
	env := desktopTestEnv(t, "")
	c := captureEmitter(t)
	dir := t.TempDir()
	if r := openPreviewTool(env, dir, ""); !strings.Contains(r, "directories are not previewable") {
		t.Fatal(r)
	}
	if r := openPreviewTool(env, "file://"+dir, ""); !strings.Contains(r, "is a directory") {
		t.Fatal(r)
	}
	if r := openPreviewTool(env, "", ""); !strings.Contains(r, "url is required") {
		t.Fatal(r)
	}
	r := openPreviewTool(env, "cnn.com", " News ")
	if mustJSON(t, r)["url"] != "https://cnn.com" || c.payload["label"] != "News" || c.event != "preview.open" {
		t.Fatal(r)
	}
	if r := previewClose(env, ""); mustJSON(t, r)["closed"] != "all" {
		t.Fatal(r)
	}
}

func TestPreviewActionsAndDriveValidation(t *testing.T) {
	env := desktopTestEnv(t, "")
	call := &Call{Env: env, Args: map[string]any{"action": "read"}}
	if r := handlePreview(context.Background(), call); !strings.Contains(r, "preview read must run inside a desktop session") {
		t.Fatal(r)
	}
	read := func(window map[string]int) (string, error) {
		return `{"kind":"browser","start":` + itoa(window["start"]) + `}`, nil
	}
	if r := ReadPreviewTool(float64(-5), "3", read); mustJSON(t, r)["start"] != float64(0) {
		t.Fatal(r)
	}
	call.Args = map[string]any{"action": "bogus"}
	if r := handlePreview(context.Background(), call); !strings.Contains(r, "action must be one of: open, close, read.") {
		t.Fatal(r)
	}
	drive := &Call{Env: env, Args: map[string]any{"action": "click"}}
	if r := handleDrivePreview(context.Background(), drive); !strings.Contains(r, "only available in the Icebath desktop app") {
		t.Fatal(r)
	}
	var got map[string]any
	act := func(payload map[string]any) (string, error) { got = payload; return `{"ok":true}`, nil }
	if r := DrivePreviewTool(drive.Args, act); !strings.Contains(r, "click needs a ref") {
		t.Fatal(r)
	}
	drive.Args = map[string]any{"action": "type", "ref": "btn"}
	if r := DrivePreviewTool(drive.Args, act); !strings.Contains(r, "type needs the text to enter.") {
		t.Fatal(r)
	}
	drive.Args = map[string]any{"action": "scroll", "amount": "12", "to": "top", "max": 2.7}
	if r := DrivePreviewTool(drive.Args, act); mustJSON(t, r)["ok"] != true || got["amount"] != 12 || got["max"] != 2 {
		t.Fatalf("%s %v", r, got)
	}
	drive.Args = map[string]any{"action": "scroll", "amount": "lots"}
	if r := DrivePreviewTool(drive.Args, act); !strings.Contains(r, "amount and max must be integers.") {
		t.Fatal(r)
	}
	act = func(payload map[string]any) (string, error) { return "", nil }
	drive.Args = map[string]any{"action": "elements"}
	if r := DrivePreviewTool(drive.Args, act); !strings.Contains(r, "No GUI window answered with a page") {
		t.Fatal(r)
	}
	ann := AnnotatePreviewTool("", nil, nil, nil, act)
	if !strings.Contains(ann, "add needs a ref") {
		t.Fatal(ann)
	}
	act = func(payload map[string]any) (string, error) { got = payload; return "plain", nil }
	label := "cheap"
	if r := AnnotatePreviewTool("clear", nil, nil, &label, act); mustJSON(t, r)["text"] != "plain" || got["action"] != "unpin" || got["text"] != "cheap" {
		t.Fatalf("%s %v", r, got)
	}
}

func TestTourValidationAndDispatch(t *testing.T) {
	env := desktopTestEnv(t, "")
	call := &Call{Env: env, Args: map[string]any{"action": "start"}}
	if r := handleTour(context.Background(), call); !strings.Contains(r, "tour is only available") {
		t.Fatal(r)
	}
	var got map[string]any
	request := func(payload map[string]any) (string, error) { got = payload; return `{"shown":true}`, nil }
	call.Args = map[string]any{"action": "start", "surface": "preview"}
	if r := TourTool(call.Args, request); !strings.Contains(r, "built-in tour runs on surface='app'") {
		t.Fatal(r)
	}
	call.Args = map[string]any{"action": "start", "steps": []any{"x"}}
	if r := TourTool(call.Args, request); !strings.Contains(r, "steps[0] must be an object.") {
		t.Fatal(r)
	}
	call.Args = map[string]any{"action": "start", "steps": []any{map[string]any{}}}
	if r := TourTool(call.Args, request); !strings.Contains(r, "steps[0] needs a selector and/or title/text.") {
		t.Fatal(r)
	}
	call.Args = map[string]any{"action": "start", "preset": "quick", "steps": []any{map[string]any{"text": "x"}}}
	if r := TourTool(call.Args, request); !strings.Contains(r, "pass steps or preset, not both.") {
		t.Fatal(r)
	}
	call.Args = map[string]any{"action": "show"}
	if r := TourTool(call.Args, request); !strings.Contains(r, "show needs a selector") {
		t.Fatal(r)
	}
	call.Args = map[string]any{"action": "Start", "preset": "quick", "step_index": float64(2)}
	if r := TourTool(call.Args, request); mustJSON(t, r)["shown"] != true || got["action"] != "start" || got["surface"] != "app" || got["preset"] != "quick" || got["step_index"] != float64(2) {
		t.Fatalf("%s %v", r, got)
	}
}

func TestSetThemeValidation(t *testing.T) {
	desktopTestEnv(t, "")
	s := func(v string) *string { return &v }
	if r := SetThemeTool(s("red"), nil, nil, nil, nil); !strings.Contains(r, "only available in the Icebath desktop app") {
		t.Fatal(r)
	}
	var got map[string]any
	request := func(values map[string]any) (map[string]any, error) {
		got = values
		return map[string]any{"applied": values}, nil
	}
	if r := SetThemeTool(nil, nil, nil, nil, request); !strings.Contains(r, "Pass at least one of accent, mode or layout") {
		t.Fatal(r)
	}
	if r := SetThemeTool(s("red"), nil, nil, s("accent"), request); !strings.Contains(r, "pass picker alone") {
		t.Fatal(r)
	}
	if r := SetThemeTool(nil, nil, nil, s("font"), request); !strings.Contains(r, "picker must be one of: accent, layout.") {
		t.Fatal(r)
	}
	if r := SetThemeTool(nil, s("dim"), nil, nil, request); !strings.Contains(r, `Unknown mode 'dim'.`) {
		t.Fatal(r)
	}
	r := SetThemeTool(s("#12abEF"), s(" dark "), nil, nil, request)
	if got["accent"] != "#12abEF" || got["mode"] != "dark" || !strings.Contains(r, "applied") {
		t.Fatalf("%s %v", r, got)
	}
	bad := mustJSON(t, SetThemeTool(s("plaid"), nil, nil, nil, request))
	if bad["success"] != false || !strings.Contains(bad["error"].(string), "Unknown accent 'plaid'") || len(bad["available"].([]any)) != 9 {
		t.Fatal(bad)
	}
	if r := SetThemeTool(nil, nil, nil, s("layout"), request); got["picker"] != "layout" || r == "" {
		t.Fatal(r)
	}
}
