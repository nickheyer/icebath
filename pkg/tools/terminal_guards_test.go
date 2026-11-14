package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// test_terminal_trailing_amp_auto_background.py
func TestTrailingAmpRunsAsTrackedBackgroundThroughTheSameGuards(t *testing.T) {
	dir := t.TempDir()
	env := newTerminalTestEnv(t, dir)
	marker := filepath.Join(dir, "ran")
	var seen []string
	checkAllGuardsFn = func(_ context.Context, _ *Env, command, _ string, _ bool) GuardResult {
		seen = append(seen, command)
		return GuardResult{Approved: true}
	}
	spawner := &fakeSpawner{}
	terminalRegistry = func() processSpawner { return spawner }
	cacheFakeEnv("default", newFakeTermEnv(dir, nil))
	r := runTerminal(t, env, TerminalRequest{Command: "echo x >> " + marker + " && sleep 0.2 &"})
	if r["error"] != nil {
		t.Fatalf("error %v", r)
	}
	if !strings.HasPrefix(resultString(r, "session_id"), "proc_") {
		t.Fatalf("session %v", r)
	}
	mustContain(t, resultString(r, "auto_backgrounded"), "trailing & was converted to background=true")
	if resultHas(r, "hint") {
		t.Fatalf("silent hint present: %v", r)
	}
	want := "echo x >> " + marker + " && sleep 0.2"
	if len(seen) != 1 || seen[0] != want {
		t.Fatalf("guard saw %v", seen)
	}
	if spawner.commands[0] != want {
		t.Fatalf("spawned %q", spawner.commands[0])
	}
}

func TestAmbiguousBackgroundingIsStillRefusedUnchanged(t *testing.T) {
	env := newTerminalTestEnv(t, "")
	acquireEnvFn = func(context.Context, *Env, *execPlan, string) (Environment, error) {
		t.Fatal("must not run")
		return nil, nil
	}
	for _, tc := range []struct{ command, expected string }{
		{"python3 -m http.server 8765 & curl -s localhost:8765", "'&' backgrounding"},
		{"sleep 1 & sleep 2 &", "'&' backgrounding"},
		{"nohup ./worker.sh > /dev/null 2>&1 &", "nohup/disown/setsid"},
		{"python3 -m http.server 8765", "long-lived server"},
		{"icebath gateway run &", "'&' backgrounding"},
	} {
		r := runTerminal(t, env, TerminalRequest{Command: tc.command})
		mustContain(t, resultString(r, "error"), tc.expected)
	}
}

func TestConvertedCommandNeverGetsRelaunchHintEvenWithAnotherHint(t *testing.T) {
	dir := t.TempDir()
	env := newTerminalTestEnv(t, dir)
	terminalRegistry = func() processSpawner { return &fakeSpawner{} }
	cacheFakeEnv("default", newFakeTermEnv(dir, nil))
	r := runTerminal(t, env, TerminalRequest{Command: "echo statusCheckRollup >/dev/null &"})
	if r["error"] != nil {
		t.Fatalf("error %v", r)
	}
	mustContain(t, resultString(r, "auto_backgrounded"), "trailing & was converted to background=true")
	mustContain(t, resultString(r, "hint"), homebrewCIPollerHint)
	if strings.Contains(resultString(r, "hint"), silentBackgroundHint) {
		t.Fatal("silent background hint leaked into a converted command")
	}
}

func TestConversionNeverChangesWhatRunsOrHowItIsApproved(t *testing.T) {
	for _, command := range []string{
		`pid="$(sleep 60 >/dev/null 2>&1 & echo $!)" && printf "%s" "$pid" &`,
		"pid=\"`sleep 60 >/dev/null 2>&1 & echo $!`\" && printf \"%s\" \"$pid\" &",
		"icebath gateway run && cp src .env &",
		"printf '<%s>' trailing\\ &",
	} {
		stripped := trailingAmpBackgroundCommand(command)
		if stripped == "" {
			continue
		}
		if strings.Contains(stripped, "$(") || strings.Contains(stripped, "`") {
			t.Fatalf("substitution survived: %q", stripped)
		}
		k1, d1, ok1 := DetectDangerousCommand(stripped)
		k2, d2, ok2 := DetectDangerousCommand(command)
		if k1 != k2 || d1 != d2 || ok1 != ok2 {
			t.Fatalf("approval identity changed for %q", command)
		}
		if _, err := exec.LookPath("bash"); err == nil {
			out := func(script string) string {
				b, _ := exec.Command("bash", "-c", script).Output()
				return string(b)
			}
			if out(stripped) != out(command+" wait") {
				t.Fatalf("output differs for %q", command)
			}
		}
	}
}

// test_terminal_heredoc_background_guard.py
func TestHeredocBackgroundGuard(t *testing.T) {
	const amp, nl = "&", "\n"
	allowed := []string{
		"osascript <<'EOF'" + nl + `set out to "count " ` + amp + " (count of items)" + nl + "EOF",
		"python3 <<'EOF'" + nl + "z = a " + amp + " b" + nl + "print(z)" + nl + "EOF",
		`cat <<"EOF"` + nl + "foo " + amp + " bar" + nl + "EOF",
		"cat <<-'EOF'" + nl + "\tfoo " + amp + " bar" + nl + "\tEOF",
		"python3 - <<'END.X'" + nl + "mode = current " + amp + " mask" + nl + "END.X",
		"python3 - <<'A' 3<<'B'" + nl + "one " + amp + " two" + nl + "A" + nl + "three " + amp + " four" + nl + "B",
		"FOO=1 env /usr/bin/python3.12 - <<'PY'" + nl + "x = a " + amp + " b" + nl + "PY",
		"cd /tmp && python3 - <<'PY'" + nl + "value = left " + amp + " right" + nl + "PY",
		"echo 'ready to run'; python3 - <<'PY'" + nl + "value = left " + amp + " right" + nl + "PY",
		"printf ignored | python3 - <<'PY'" + nl + "value = left " + amp + " right" + nl + "PY",
		"python3 - <<'PY' 2>&1" + nl + "value = left " + amp + " right" + nl + "PY",
		"python3 - <<'PY' &>/tmp/python.log" + nl + "value = left " + amp + " right" + nl + "PY",
	}
	for _, cmd := range allowed {
		if g := foregroundBackgroundGuidance(cmd); g != "" {
			t.Fatalf("inert heredoc flagged: %q -> %s", cmd, g)
		}
	}
	blocked := []string{
		"cat <<EOF" + nl + "foo " + amp + " bar" + nl + "EOF",
		"cat <<EOF" + nl + "$(nohup sleep 10 >/dev/null 2>" + amp + "1 " + amp + ")" + nl + "EOF",
		"bash <<'EOF'" + nl + "nohup sleep 10 >/dev/null 2>" + amp + "1 " + amp + nl + "EOF",
		"python3 -c 'pass'; bash <<'EOF'" + nl + "nohup sleep 10 " + amp + nl + "EOF",
		"bash <<'EOF' | python3" + nl + "nohup sleep 10 " + amp + nl + "EOF",
		"cat <<'EOF' | sh" + nl + "nohup sleep 10 " + amp + nl + "EOF",
		"python3 -c $(bash <<'SH'" + nl + "nohup sleep 100 >/dev/null 2>" + amp + "1 " + amp + nl + "printf pass" + nl + "SH" + nl + ")",
		"python3() { bash; }; python3 <<'PY'" + nl + "nohup sleep 10 " + amp + nl + "PY",
		": # <<EOF" + nl + "nohup sleep 10 " + amp,
		"printf '<<EOF" + nl + "literal'" + nl + "sleep 100 " + amp,
		"cat <<<EOF" + nl + "nohup sleep 10 " + amp,
		"python3 - <<'PY'" + nl + "sleep 100 " + amp,
		"python3 - <<'PY' \\" + nl + "  >/dev/null " + amp + nl + "print('ok')" + nl + "PY",
		"python3 server.py " + amp,
		"sleep 100 " + amp + " echo done",
		"python3 - <<'PY' " + amp + nl + "print('ok')" + nl + "PY",
		"python3 - <<'PY' 2>" + amp + "1 " + amp + nl + "print('ok')" + nl + "PY",
		"python3 - <<'PY'" + nl + "print('ok')" + nl + "PY" + nl + "long_running " + amp,
	}
	for _, cmd := range blocked {
		if foregroundBackgroundGuidance(cmd) == "" {
			t.Fatalf("background operator hidden: %q", cmd)
		}
	}
}

// test_terminal_tool.py workdir cases
func TestValidateWorkdir(t *testing.T) {
	for _, bad := range []string{
		`C:\Users\Alice\project; rm -rf /`, `C:\Users\Alice\project$(whoami)`, "C:\\Users\\Alice\\project\nwhoami",
		"/tmp/\u30c6\u30b9\u30c8; rm -rf /", "/tmp/\u9879\u76ee$(whoami)", "/tmp/\u00fcber`id`",
		"/tmp/\u30c6\u30b9\u30c8\nwhoami", "/tmp/\u9879\u76ee|cat /etc/passwd", "/tmp/\u00fc\x00ber",
	} {
		if validateWorkdir(bad) == "" {
			t.Fatalf("allowed %q", bad)
		}
	}
	for _, good := range []string{
		"/Users/alice/Documents/Obs_Hermes_Data/\u9879\u76ee-projects/\u5ba2\u6237\u62dc\u8bbf", "/tmp/\u30c6\u30b9\u30c8", "/home/j\u00fcrgen/\u00fcber projekt",
	} {
		if msg := validateWorkdir(good); msg != "" {
			t.Fatalf("blocked %q: %s", good, msg)
		}
	}
}

func TestBlockedJSONEnvelope(t *testing.T) {
	r := map[string]any{}
	if err := json.Unmarshal([]byte(blockedJSON("Blocked: x", "blocked")), &r); err != nil {
		t.Fatal(err)
	}
	if r["output"] != "" || r["exit_code"] != float64(1) || r["error"] != "Blocked: x" || r["status"] != "blocked" {
		t.Fatalf("envelope %v", r)
	}
}

func TestReadScriptForGuardSkipsBinaryAndReadsText(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.sh"), []byte("echo ok\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.bin"), []byte("ab\x00cd"), 0o644)
	e := newFakeTermEnv(dir, func(_ context.Context, command string, _ ExecOptions) (*ExecResult, error) {
		return &ExecResult{Output: "remote\n"}, nil
	})
	if text, ok := readScriptForGuard(context.Background(), e, dir, "a.sh", 1024); !ok || text != "echo ok\n" {
		t.Fatalf("local read %q %v", text, ok)
	}
	if _, ok := readScriptForGuard(context.Background(), e, dir, "b.bin", 1024); ok {
		t.Fatal("binary content must not be a script")
	}
	if text, ok := readScriptForGuard(context.Background(), e, dir, "/remote/missing.sh", 1024); !ok || text != "remote\n" {
		t.Fatalf("remote read %q %v", text, ok)
	}
	if len(e.calls) != 1 || !strings.HasPrefix(e.calls[0].command, "head -c 1025 < ") {
		t.Fatalf("remote command %v", e.calls)
	}
}
