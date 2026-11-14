//go:build linux

package pty_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/connections"
	"github.com/nickheyer/icebath/pkg/desktop/pty"
)

type plainSafe struct{}

func (plainSafe) IsEncryptionAvailable() bool                    { return false }
func (plainSafe) EncryptString(plaintext string) ([]byte, error) { return []byte(plaintext), nil }
func (plainSafe) DecryptString(ciphertext []byte) (string, error) {
	return string(ciphertext), nil
}
func (plainSafe) SetUsePlainTextEncryption(bool) {}

type event struct {
	name    string
	payload any
}

type window struct {
	id        string
	mu        sync.Mutex
	events    []event
	destroyed []func()
}

func (w *window) ID() string { return w.id }

func (w *window) Send(name string, payload any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, event{name, payload})
}

func (w *window) OnDestroyed(f func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.destroyed = append(w.destroyed, f)
}

func (w *window) output(id string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var b strings.Builder
	for _, e := range w.events {
		if e.name == "terminal:"+id+":data" {
			b.WriteString(e.payload.(string))
		}
	}
	return b.String()
}

func (w *window) exits(id string) []pty.ExitPayload {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []pty.ExitPayload
	for _, e := range w.events {
		if e.name == "terminal:"+id+":exit" {
			out = append(out, e.payload.(pty.ExitPayload))
		}
	}
	return out
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type harness struct {
	svc      *pty.Service
	main     *window
	home     string
	userHome string
	userData string
	work     string
	logs     *[]string
}

// Terminal service over a real connections service with an empty registry under a temp ICEBATH_HOME
func newHarness(t *testing.T) harness {
	t.Helper()
	root := t.TempDir()
	h := harness{home: filepath.Join(root, "icebath"), userHome: filepath.Join(root, "user"), userData: filepath.Join(root, "userdata"), work: filepath.Join(root, "work"), logs: &[]string{}}
	for _, dir := range []string{h.home, h.userHome, h.userData, h.work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h.home)
	t.Setenv("HOME", h.userHome)
	t.Setenv("ICEBATH_DESKTOP_SHELL", "/bin/sh")
	t.Setenv("ICEBATH_DESKTOP_REMOTE_URL", "")
	t.Setenv("ICEBATH_DESKTOP_REMOTE_TOKEN", "")
	conns := connections.NewService(connections.ServiceOptions{Platform: "linux", IcebathHome: h.home, UserData: h.userData,
		Safe: plainSafe{}, Log: func(string) {}, Broadcast: func(string, any) {}})
	logs := h.logs
	h.svc = pty.NewService(pty.Options{
		Platform: "linux", HomeDir: func() string { return h.userHome }, UserData: h.userData, IcebathHome: h.home,
		AppVersion: func() string { return "9.8.7" }, Log: func(line string) { *logs = append(*logs, line) },
		Routes: conns, WindowRoutes: backend.NewWindowConnectionRoutes(), PrimaryProfile: func() string { return "default" },
		Resolve: func(args []string) (backend.ResolvedBackend, error) {
			return backend.ResolvedBackend{BackendCommand: backend.BackendCommand{Command: "/opt/icebath/bin/icebath", Args: args,
				Env: map[string]string{"PATH": "/nowhere", "EMPTY": "", "ICEBATH_RUNTIME_DIR": "/opt/rt"}}}, nil
		},
		SanitizeCwd: func(cwd any) string { s, _ := cwd.(string); return s },
	})
	h.main = &window{id: "main"}
	t.Cleanup(h.svc.DisposeAllTerminalSessions)
	return h
}

func (h harness) start(t *testing.T, payload map[string]any) pty.StartResult {
	t.Helper()
	started, err := h.svc.TerminalStart(h.main, payload)
	if err != nil {
		t.Fatal(err)
	}
	return started
}

func shellPID(t *testing.T, h harness, id string) int {
	t.Helper()
	h.svc.TerminalWrite(id, "echo pid=$$=\n")
	var pid int
	eventually(t, "the shell pid", func() bool {
		out := h.main.output(id)
		i := strings.LastIndex(out, "pid=")
		if i < 0 {
			return false
		}
		rest := out[i+4:]
		j := strings.Index(rest, "=")
		if j <= 0 {
			return false
		}
		n, err := strconv.Atoi(rest[:j])
		pid = n
		return err == nil
	})
	return pid
}

func TestTerminalStartSpawnsTheShellWhereAskedAndReportsARemovedGateway(t *testing.T) {
	h := newHarness(t)
	t.Setenv("NO_COLOR", "1")
	started := h.start(t, map[string]any{"cwd": h.work, "cols": float64(100), "rows": "30"})
	if started.ID == "" || started.Shell != "sh" || started.Cwd == nil || *started.Cwd != h.work || started.ConnectionID == nil || *started.ConnectionID != "local" || started.Unavailable != "" {
		t.Fatalf("start result %+v", started)
	}
	h.svc.TerminalAttach("main", started.ID)
	h.svc.TerminalWrite(started.ID, "printf 'T=%s P=%s V=%s D=%s C=%s\\n' \"$TERM\" \"$TERM_PROGRAM\" \"$TERM_PROGRAM_VERSION\" \"$ICEBATH_DESKTOP_TERMINAL\" \"${NO_COLOR-unset}\"; stty size; pwd\n")
	eventually(t, "the shell environment and size", func() bool {
		out := h.main.output(started.ID)
		return strings.Contains(out, "T=xterm-256color P=Icebath V=9.8.7 D=1 C=unset") && strings.Contains(out, "30 100") && strings.Contains(out, h.work)
	})
	file := filepath.Join(h.work, "notes.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := h.start(t, map[string]any{"cwd": file}); got.Cwd == nil || *got.Cwd != h.work {
		t.Fatalf("a file cwd started in %+v", got)
	}
	if got := h.start(t, map[string]any{"cwd": filepath.Join(h.work, "absent")}); got.Cwd == nil || *got.Cwd != h.userHome {
		t.Fatalf("a missing cwd started in %+v", got)
	}
	missing := h.start(t, map[string]any{"connectionId": " gone ", "profile": "work"})
	if missing.Unavailable != "missing" || missing.ID != "" || missing.Shell != "" || missing.Cwd != nil || missing.ConnectionID == nil || *missing.ConnectionID != "gone" {
		t.Fatalf("removed gateway answered %+v", missing)
	}
}

func TestTerminalAttachFlushesBufferedOutputOnlyToTheStartingWindow(t *testing.T) {
	h := newHarness(t)
	started := h.start(t, map[string]any{"cwd": h.work})
	h.svc.TerminalWrite(started.ID, "echo buffered-$((40+2)); : > ran\n")
	eventually(t, "the shell to run the command", func() bool {
		_, err := os.Stat(filepath.Join(h.work, "ran"))
		return err == nil
	})
	if h.main.output(started.ID) != "" {
		t.Fatal("output reached the renderer before it attached")
	}
	if h.svc.TerminalAttach("other", started.ID) || h.svc.TerminalAttach("main", "unknown") {
		t.Fatal("attach accepted another window or an unknown session")
	}
	if !h.svc.TerminalAttach("main", started.ID) {
		t.Fatal("the starting window could not attach")
	}
	if !strings.Contains(h.main.output(started.ID), "buffered-42") {
		t.Fatalf("attach did not flush the buffered output %q", h.main.output(started.ID))
	}
	if !h.svc.TerminalAttach("main", started.ID) || strings.Count(h.main.output(started.ID), "buffered-42") != 1 {
		t.Fatal("a second attach was refused or replayed the buffer")
	}
}

func TestTerminalWriteFeedsTheShellAndRefusesAGoneSession(t *testing.T) {
	h := newHarness(t)
	started := h.start(t, map[string]any{"cwd": h.work})
	h.svc.TerminalAttach("main", started.ID)
	if !h.svc.TerminalWrite(started.ID, "echo written-$((6*7))\n") {
		t.Fatal("write to a live session was refused")
	}
	eventually(t, "the written command output", func() bool { return strings.Contains(h.main.output(started.ID), "written-42") })
	if h.svc.TerminalWrite("unknown", "echo x\n") || h.svc.TerminalWrite(nil, "echo x\n") {
		t.Fatal("write to a missing session was accepted")
	}
}

func TestTerminalResizeClampsAndAppliesTheSize(t *testing.T) {
	h := newHarness(t)
	started := h.start(t, map[string]any{"cwd": h.work})
	h.svc.TerminalAttach("main", started.ID)
	if ok, err := h.svc.TerminalResize(started.ID, map[string]any{"cols": float64(120), "rows": float64(40)}); !ok || err != nil {
		t.Fatalf("resize answered %v %v", ok, err)
	}
	h.svc.TerminalWrite(started.ID, "stty size\n")
	eventually(t, "the resized terminal", func() bool { return strings.Contains(h.main.output(started.ID), "40 120") })
	if ok, err := h.svc.TerminalResize(started.ID, map[string]any{"cols": "abc", "rows": float64(1)}); !ok || err != nil {
		t.Fatalf("clamped resize answered %v %v", ok, err)
	}
	h.svc.TerminalWrite(started.ID, "stty size\n")
	eventually(t, "the clamped terminal", func() bool { return strings.Contains(h.main.output(started.ID), "2 80") })
	if ok, err := h.svc.TerminalResize("unknown", map[string]any{"cols": float64(90)}); ok || err != nil {
		t.Fatalf("resize of a missing session answered %v %v", ok, err)
	}
}

func TestTerminalCwdFollowsTheLiveShell(t *testing.T) {
	h := newHarness(t)
	started := h.start(t, map[string]any{"cwd": h.work})
	h.svc.TerminalWrite(started.ID, "mkdir sub; cd sub\n")
	eventually(t, "the shell to change directory", func() bool {
		cwd := h.svc.TerminalCwd(started.ID)
		return cwd != nil && *cwd == filepath.Join(h.work, "sub")
	})
	if h.svc.TerminalCwd("unknown") != nil {
		t.Fatal("a missing session reported a cwd")
	}
}

func TestTerminalDisposeHangsUpTheShellAndForgetsTheSession(t *testing.T) {
	h := newHarness(t)
	started := h.start(t, map[string]any{"cwd": h.work})
	h.svc.TerminalAttach("main", started.ID)
	pid := shellPID(t, h, started.ID)
	if !h.svc.TerminalDispose(started.ID) {
		t.Fatal("dispose of a live session answered false")
	}
	eventually(t, "the hung up shell", func() bool { return len(h.main.exits(started.ID)) == 1 })
	if exit := h.main.exits(started.ID)[0]; exit.Code == nil || *exit.Code != 0 || exit.Signal == nil || *exit.Signal != "1" {
		t.Fatalf("hang up exit %+v", exit)
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err == nil {
		t.Fatalf("shell %d still runs", pid)
	}
	if h.svc.TerminalDispose(started.ID) || h.svc.TerminalWrite(started.ID, "echo x\n") {
		t.Fatal("a disposed session answered")
	}

	quitting := h.start(t, map[string]any{"cwd": h.work})
	h.svc.TerminalAttach("main", quitting.ID)
	qpid := shellPID(t, h, quitting.ID)
	h.svc.DisposeAllTerminalSessions()
	eventually(t, "the quit to end the shell", func() bool {
		_, err := os.Stat("/proc/" + strconv.Itoa(qpid))
		return err != nil
	})
	time.Sleep(500 * time.Millisecond)
	if len(h.main.exits(quitting.ID)) != 0 || h.svc.TerminalWrite(quitting.ID, "echo x\n") {
		t.Fatal("app quit reported the shell exit or kept the session")
	}

	gone := h.start(t, map[string]any{"cwd": h.work})
	for _, f := range h.main.destroyed {
		f()
	}
	if h.svc.TerminalWrite(gone.ID, "echo x\n") {
		t.Fatal("a destroyed window kept its session")
	}
}

func TestTerminalOnDataStreamsLiveOutputAsWholeText(t *testing.T) {
	h := newHarness(t)
	started := h.start(t, map[string]any{"cwd": h.work})
	h.svc.TerminalAttach("main", started.ID)
	h.svc.TerminalWrite(started.ID, "printf 'live-\\303\\251\\342\\202\\254-done\\n'\n")
	want := "live-" + string([]rune{0xE9, 0x20AC}) + "-done"
	eventually(t, "the live output", func() bool { return strings.Contains(h.main.output(started.ID), want) })
	h.main.mu.Lock()
	defer h.main.mu.Unlock()
	for _, e := range h.main.events {
		if s, ok := e.payload.(string); ok && strings.ContainsRune(s, 0xFFFD) {
			t.Fatalf("a data chunk split a UTF-8 sequence %q", s)
		}
	}
}

func TestTerminalOnExitReportsCodeAndSignalThenForgetsTheSession(t *testing.T) {
	h := newHarness(t)
	started := h.start(t, map[string]any{"cwd": h.work})
	h.svc.TerminalAttach("main", started.ID)
	h.svc.TerminalWrite(started.ID, "exit 7\n")
	eventually(t, "the exit", func() bool { return len(h.main.exits(started.ID)) == 1 })
	if exit := h.main.exits(started.ID)[0]; exit.Code == nil || *exit.Code != 7 || exit.Signal == nil || *exit.Signal != "0" {
		t.Fatalf("exit payload %+v", exit)
	}
	if h.svc.TerminalWrite(started.ID, "echo late\n") || h.svc.TerminalDispose(started.ID) {
		t.Fatal("a flushed exit left the session behind")
	}

	late := h.start(t, map[string]any{"cwd": h.work})
	h.svc.TerminalWrite(late.ID, "exit 3\n")
	eventually(t, "the unattached shell to end", func() bool { return h.svc.TerminalCwd(late.ID) == nil })
	time.Sleep(300 * time.Millisecond)
	if len(h.main.exits(late.ID)) != 0 || !h.svc.TerminalWrite(late.ID, "") {
		t.Fatal("an unattached exit was delivered early or dropped the session")
	}
	if !h.svc.TerminalAttach("main", late.ID) {
		t.Fatal("late attach was refused")
	}
	if exits := h.main.exits(late.ID); len(exits) != 1 || exits[0].Code == nil || *exits[0].Code != 3 {
		t.Fatalf("late attach exits %+v", exits)
	}
	if h.svc.TerminalWrite(late.ID, "echo x\n") {
		t.Fatal("the late attach left the session behind")
	}
}

func TestOpenSessionInTerminalWritesTheLauncherAndStartsTheUserTerminal(t *testing.T) {
	h := newHarness(t)
	bin := filepath.Join(filepath.Dir(h.home), "bin")
	captured := filepath.Join(filepath.Dir(h.home), "launched")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + captured + ".tmp && /bin/mv " + captured + ".tmp " + captured + "\n"
	if err := os.WriteFile(filepath.Join(bin, "x-terminal-emulator"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if got := h.svc.OpenSessionInTerminal("  ", nil); got.OK || got.Error != "invalid-session-id" {
		t.Fatalf("blank session id answered %+v", got)
	}
	if got := h.svc.OpenSessionInTerminal(" sess-1 ", map[string]any{"profile": " Work ", "cwd": h.work}); !got.OK {
		t.Fatalf("open in terminal answered %+v", got)
	}
	var argv []string
	eventually(t, "the terminal emulator launch", func() bool {
		raw, err := os.ReadFile(captured)
		argv = strings.Split(strings.TrimSpace(string(raw)), "\n")
		return err == nil
	})
	if len(argv) != 3 || argv[0] != "-e" || argv[1] != "/bin/sh" || !strings.HasPrefix(argv[2], filepath.Join(h.userData, "open-in-terminal", "icebath-")) || !strings.HasSuffix(argv[2], ".sh") {
		t.Fatalf("terminal emulator argv %q", argv)
	}
	script, err := os.ReadFile(argv[2])
	if err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/sh\ncd '" + h.work + "' || exit 1\nexport ICEBATH_RUNTIME_DIR='/opt/rt'\nexport ICEBATH_HOME='" + h.home + "'\n" +
		"exec '/opt/icebath/bin/icebath' '--profile' 'work' '--tui' '--resume' 'sess-1'\n"
	if string(script) != want {
		t.Fatalf("launcher script\n%s\nwant\n%s", script, want)
	}
	logs := *h.logs
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "[terminal] opening session  sess-1  via "+filepath.Join(bin, "x-terminal-emulator")) {
		t.Fatalf("desktop log %q", logs)
	}
	t.Setenv("PATH", filepath.Join(filepath.Dir(h.home), "empty"))
	if got := h.svc.OpenSessionInTerminal("sess-2", nil); got.OK || got.Error != "No terminal emulator found" {
		t.Fatalf("no emulator answered %+v", got)
	}
}
