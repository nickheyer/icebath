package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

var warpOKEnv = map[string]string{
	"TERM_PROGRAM":                    "WarpTerminal",
	"WARP_CLI_AGENT_PROTOCOL_VERSION": "1",
	"WARP_CLIENT_VERSION":             "v0.2026.08.01.00.00.stable_01",
}

// Rings the prompt bell under the flag and env and returns what reached the tty
func ringForTest(t *testing.T, flagOn bool, env map[string]string, text, detail string) string {
	t.Helper()
	for key := range warpOKEnv {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
	var written strings.Builder
	prev := writeTTY
	writeTTY = func(seq string) { written.WriteString(seq) }
	t.Cleanup(func() { writeTTY = prev })
	c := &CLI{ctx: context.Background(), bellOnPrompt: flagOn, sessionID: "sess-1"}
	c.ringBell(true, text, detail)
	return written.String()
}

func TestOsc9BodyEmittedAndSanitizedOnlyWhenFlagOn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := ringForTest(t, true, nil, "approval\x1b\x07\x00\x7f!", "")
	if out != "\a\x1b]9;Icebath: approval!\x07" {
		t.Fatalf("got %q", out)
	}
	if got := ringForTest(t, false, nil, "approval", ""); got != "" {
		t.Fatalf("flag off wrote %q", got)
	}
}

func TestWarpOsc777OnlyUnderSupportedWarpBuild(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := ringForTest(t, true, warpOKEnv, "approval", "rm -rf build")
	prefix := "\x1b]777;notify;warp://cli-agent;"
	if strings.Count(out, prefix) != 1 {
		t.Fatalf("got %q", out)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.SplitN(out, prefix, 2)[1], "\x07")), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["agent"] != "icebath" || payload["event"] != "permission_request" || payload["summary"] != "rm -rf build" ||
		payload["session_id"] != "sess-1" || payload["v"] != float64(1) {
		t.Fatalf("payload %v", payload)
	}
	broken := map[string]string{}
	for k, v := range warpOKEnv {
		broken[k] = v
	}
	broken["WARP_CLIENT_VERSION"] = "v0.2026.03.25.08.24.stable_05"
	if strings.Contains(ringForTest(t, true, broken, "approval", ""), prefix) {
		t.Fatal("a broken Warp build must get OSC 9 only")
	}
	notWarp := map[string]string{}
	for k, v := range warpOKEnv {
		notWarp[k] = v
	}
	notWarp["TERM_PROGRAM"] = "ghostty"
	if strings.Contains(ringForTest(t, true, notWarp, "approval", ""), prefix) {
		t.Fatal("a non Warp terminal must get OSC 9 only")
	}
}

func TestRunningAppGetsBellAndOsc9OnItsLoopNeverASecondTtyWriter(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for key := range warpOKEnv {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	prev := writeTTY
	writeTTY = func(seq string) { t.Errorf("stray tty write: %q", seq) }
	t.Cleanup(func() { writeTTY = prev })
	app := &fakeTUIApp{running: true, loop: true}
	c := &CLI{ctx: context.Background(), bellOnComplete: true, sessionID: "sess-1", app: app}
	c.ringBell(false, "turn complete", "")
	if len(app.rawWrites()) != 0 {
		t.Fatalf("wrote before the loop ran: %v", app.rawWrites())
	}
	queued := app.loopQueue()
	if len(queued) != 1 {
		t.Fatalf("queued %d callbacks", len(queued))
	}
	queued[0]()
	if got := app.rawWrites(); len(got) != 2 || got[0] != "\a\x1b]9;Icebath: turn complete\x07" || got[1] != "<flush>" {
		t.Fatalf("got %q", got)
	}
}
