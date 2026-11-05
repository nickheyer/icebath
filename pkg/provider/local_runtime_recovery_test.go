package provider

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Scripted process whose nil hooks answer like a live untouched psutil.Process
type lr4FakeProc struct {
	pid                       int
	created                   float64
	exe                       string
	argv                      []string
	running                   func() bool
	children                  func(recursive bool) ([]localRuntimeProc, error)
	terminate                 func() error
	terminated, killed, waits int
	childrenCalls             []bool
}

func (p *lr4FakeProc) Pid() int                     { return p.pid }
func (p *lr4FakeProc) CreateTime() (float64, error) { return p.created, nil }
func (p *lr4FakeProc) Exe() (string, error)         { return p.exe, nil }
func (p *lr4FakeProc) PPID() (int, error)           { return 1, nil }
func (p *lr4FakeProc) Parent() (localRuntimeProc, error) {
	return nil, nil
}
func (p *lr4FakeProc) Cmdline() ([]string, error) { return p.argv, nil }
func (p *lr4FakeProc) IsRunning() bool {
	if p.running != nil {
		return p.running()
	}
	return true
}
func (p *lr4FakeProc) Children(recursive bool) ([]localRuntimeProc, error) {
	p.childrenCalls = append(p.childrenCalls, recursive)
	if p.children != nil {
		return p.children(recursive)
	}
	return nil, nil
}
func (p *lr4FakeProc) Terminate() error {
	p.terminated++
	if p.terminate != nil {
		return p.terminate()
	}
	return nil
}
func (p *lr4FakeProc) Kill() error                      { p.killed++; return nil }
func (p *lr4FakeProc) Wait(timeout time.Duration) error { p.waits++; return nil }

// Points the recovery state file at the machine runtimes root of a temporary home
func lr4StateHome(t *testing.T) string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := RuntimesRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "server.json")
	prev := lrStatePath
	lrStatePath = func() string { return path }
	t.Cleanup(func() { lrStatePath = prev })
	return path
}

// Writes the record as JSON
func lr4WriteState(t *testing.T, path string, state map[string]any) {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Endpoint the managed state file resolves to, nil when it resolves to nothing
func lr4StateEndpoint() map[string]string {
	base, key, ok := managedLocalStateEndpoint(home.Process())
	if !ok {
		return nil
	}
	return map[string]string{"base_url": base, "api_key": key}
}

// Starts a sleeping helper process and returns it with its pid
func lr4Sleeper(t *testing.T) (*os.Process, int) {
	t.Helper()
	cmd := lr3HelperCmd(t.TempDir(), "sleeper")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process, cmd.Process.Pid
}

// Full modern record of pid owned by this test process the way the supervisor writes it
func lr4ModernRecord(t *testing.T, pid int) map[string]any {
	t.Helper()
	created, err := tools.ProcessBirth(pid)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := psExe(pid)
	if err != nil {
		t.Fatal(err)
	}
	ownerCreated, err := tools.ProcessBirth(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"pid": pid, "create_time": created, "executable": exe,
		"owner_pid": os.Getpid(), "owner_create_time": ownerCreated, "start_time": nil, "owner_start_time": nil}
	if start, ok := tools.ProcessStartTime(pid); ok {
		state["start_time"] = start
	}
	if start, ok := tools.ProcessStartTime(os.Getpid()); ok {
		state["owner_start_time"] = start
	}
	return state
}

func TestLocalRuntimeLegacyEndpointRequiresManagedProcessIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accepted bool
	}{{"managed", true}, {"foreign-executable", false}, {"pid-reused", false}} {
		t.Run(tc.name, func(t *testing.T) {
			path := lr4StateHome(t)
			root := filepath.Dir(path)
			models := filepath.Join(t.TempDir(), "models")
			prevModels := lrModelsDir
			lrModelsDir = func() string { return models }
			t.Cleanup(func() { lrModelsDir = prevModels })
			state := map[string]any{"pid": 647, "base_url": "http://127.0.0.1:18434/v1", "api_key": "legacy-test-key"}
			lr4WriteState(t, path, state)
			if err := os.Chtimes(path, time.Unix(200, 0), time.Unix(200, 0)); err != nil {
				t.Fatal(err)
			}
			managedExe := filepath.Join(root, "b10964", "metal", "llama-server")
			executable := managedExe
			if tc.name == "foreign-executable" {
				executable = filepath.Join(t.TempDir(), "usr", "sbin", "distnoted")
			}
			created := 100.0
			if tc.name == "pid-reused" {
				created = 201
			}
			process := &lr4FakeProc{pid: 647, created: created, exe: executable, argv: []string{managedExe,
				"--host", "127.0.0.1", "--port", "18434", "--api-key", "legacy-test-key", "--models-dir", models}}
			prevOpen, prevExists := lrOpenProcess, lrPidExists
			lrOpenProcess = func(pid int) (localRuntimeProc, error) { return process, nil }
			lrPidExists = func(pid int) bool { return true }
			t.Cleanup(func() { lrOpenProcess, lrPidExists = prevOpen, prevExists })

			got, err := legacyRecordedProcess(readLocalRuntimeState())
			if err != nil {
				t.Fatal(err)
			}
			if tc.accepted != (got == localRuntimeProc(process)) || (!tc.accepted && got != nil) {
				t.Fatalf("legacyRecordedProcess = %v, accepted %v", got, tc.accepted)
			}
			var want map[string]string
			if tc.accepted {
				want = map[string]string{"base_url": "http://127.0.0.1:18434/v1", "api_key": "legacy-test-key"}
			}
			if got := lr4StateEndpoint(); len(got) != len(want) || got["base_url"] != want["base_url"] || got["api_key"] != want["api_key"] {
				t.Fatalf("state endpoint = %v, want %v", got, want)
			}
		})
	}
}

func TestLocalRuntimeLegacyRecordRejectsTruthyNonStringBaseURL(t *testing.T) {
	path := lr4StateHome(t)
	root := filepath.Dir(path)
	lr4WriteState(t, path, map[string]any{"pid": 647})
	process := &lr4FakeProc{pid: 647, created: 1, exe: filepath.Join(root, "b1", "cpu", "llama-server")}
	prevOpen := lrOpenProcess
	lrOpenProcess = func(pid int) (localRuntimeProc, error) { return process, nil }
	t.Cleanup(func() { lrOpenProcess = prevOpen })
	for _, raw := range []any{nil, 0, false, "", []any{}} {
		if got, err := legacyRecordedProcess(map[string]any{"pid": 647, "base_url": raw, "api_key": "k"}); got != nil || err != nil {
			t.Fatalf("base_url %v = %v, %v", raw, got, err)
		}
	}
	var attr *lrAttributeError
	if _, err := legacyRecordedProcess(map[string]any{"pid": 647, "base_url": 123, "api_key": "k"}); !errors.As(err, &attr) {
		t.Fatalf("truthy int base_url error = %v", err)
	}
}

func TestLocalRuntimeExplicitStopPreservesVerifiedRootIncarnation(t *testing.T) {
	for _, reuseAt := range []string{"before-walk", "during-walk", "never"} {
		t.Run(reuseAt, func(t *testing.T) {
			path := lr4StateHome(t)
			if err := os.WriteFile(path, []byte(`{"pid": 123, "create_time": 1.0}`), 0o644); err != nil {
				t.Fatal(err)
			}
			stale := false
			nsp := &psError{Class: "NoSuchProcess", Pid: 123}
			ownedChild := &lr4FakeProc{pid: 200}
			replacementChild := &lr4FakeProc{pid: 201}
			root := &lr4FakeProc{pid: 123}
			root.running = func() bool {
				if stale {
					return false
				}
				stale = reuseAt == "before-walk"
				return true
			}
			root.children = func(recursive bool) ([]localRuntimeProc, error) {
				if stale {
					return nil, nsp
				}
				if reuseAt == "during-walk" {
					stale = true
					return []localRuntimeProc{replacementChild}, nil
				}
				return []localRuntimeProc{ownedChild}, nil
			}
			root.terminate = func() error {
				if stale {
					return nsp
				}
				return nil
			}
			factoryCalls := 0
			prevOpen, prevExists, prevRec, prevDead := lrOpenProcess, lrPidExists, lrRecordedProcess, lrOwnerIsDead
			lrOpenProcess = func(pid int) (localRuntimeProc, error) { factoryCalls++; return nil, nsp }
			lrPidExists = func(pid int) bool { return true }
			lrRecordedProcess = func(map[string]any) localRuntimeProc { return root }
			lrOwnerIsDead = func(map[string]any) bool { return true }
			t.Cleanup(func() {
				lrOpenProcess, lrPidExists, lrRecordedProcess, lrOwnerIsDead = prevOpen, prevExists, prevRec, prevDead
			})

			stopped, err := StopRecordedOrphan()
			if err != nil {
				t.Fatal(err)
			}
			if replacementChild.terminated+replacementChild.killed != 0 {
				t.Fatal("a replacement child was signalled")
			}
			if len(root.childrenCalls) != 1 || !root.childrenCalls[0] {
				t.Fatalf("children calls = %v, want one recursive walk", root.childrenCalls)
			}
			if factoryCalls != 0 {
				t.Fatal("the stop rebuilt a process from a pid")
			}
			if stopped != (reuseAt == "never") {
				t.Fatalf("stopped = %v", stopped)
			}
			if reuseAt != "never" {
				if root.terminated+root.killed+ownedChild.terminated+ownedChild.killed != 0 {
					t.Fatal("a stale incarnation was signalled")
				}
			} else if root.terminated != 1 || ownedChild.terminated != 1 || ownedChild.killed != 1 {
				t.Fatalf("root terminate %d, child terminate %d kill %d", root.terminated, ownedChild.terminated, ownedChild.killed)
			}
		})
	}
}

func TestLocalRuntimeStopWithoutRecordOrWithDeadPid(t *testing.T) {
	path := lr4StateHome(t)
	if stopped, err := StopRecordedOrphan(); !stopped || err != nil {
		t.Fatalf("missing record = %v, %v", stopped, err)
	}
	for _, text := range []string{"{", "[]", `{"pid": true}`, `{"pid": 0}`} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if stopped, err := StopRecordedOrphan(); stopped || err != nil {
			t.Fatalf("%s = %v, %v", text, stopped, err)
		}
	}
	if err := os.WriteFile(path, []byte(`{"pid": 4294967296}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := StopRecordedOrphan(); err == nil {
		t.Fatal("an out of range pid must raise like os.kill")
	}
	prev := lrPidExists
	lrPidExists = func(int) bool { return false }
	t.Cleanup(func() { lrPidExists = prev })
	if err := os.WriteFile(path, []byte(`{"pid": 4242}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if stopped, err := StopRecordedOrphan(); !stopped || err != nil {
		t.Fatalf("dead pid = %v, %v", stopped, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the dead record must be kept")
	}
}

func TestLocalRuntimeSteppedClockNeverBuriesALiveOwnerOfAnOlderRecord(t *testing.T) {
	created, err := tools.ProcessBirth(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"owner_pid": os.Getpid(), "owner_create_time": created}
	prev := lrBirth
	lrBirth = func(pid int) (float64, error) {
		got, err := prev(pid)
		return got - 22, err
	}
	t.Cleanup(func() { lrBirth = prev })
	if got, _ := lrBirth(os.Getpid()); got == created {
		t.Fatal("the clock step did not move create_time")
	}
	if ownerIsDead(state) {
		t.Fatal("a live owner read as dead")
	}
}

func TestLocalRuntimeRetainedEndpointValidatesIdentity(t *testing.T) {
	for _, damage := range []string{"valid", "birth", "exe", "bool-pid", "bool-birth", "nan", "inf", "owner-bool",
		"owner-nan", "parent", "partial", "list", "invalid", "unreadable"} {
		t.Run(damage, func(t *testing.T) {
			if damage == "parent" && runtime.GOOS != "windows" {
				t.Skip("POSIX reparents orphans, so only Windows rejects a recorded owner that is not the parent")
			}
			path := lr4StateHome(t)
			proc, pid := lr4Sleeper(t)
			real, err := openPSProcess(pid)
			if err != nil {
				t.Fatal(err)
			}
			state := lr4ModernRecord(t, pid)
			delete(state, "start_time")
			delete(state, "owner_start_time")
			state["base_url"], state["api_key"] = "http://127.0.0.1:59999/v1", "test-only"
			created := state["create_time"].(float64)
			text := ""
			switch damage {
			case "birth":
				state["create_time"] = created - 10
			case "exe":
				state["executable"] = filepath.Join(t.TempDir(), "wrong.exe")
			case "bool-pid":
				state["pid"] = true
			case "bool-birth":
				state["create_time"] = true
			case "owner-bool":
				state["owner_create_time"] = true
			case "parent":
				state["owner_pid"] = pid
			case "partial":
				delete(state, "create_time")
			case "list":
				text = "[]"
			case "invalid":
				text = "{"
			}
			data, _ := json.Marshal(state)
			switch damage {
			case "nan", "inf", "owner-nan":
				literal := map[string]string{"nan": "NaN", "inf": "Infinity", "owner-nan": "NaN"}[damage]
				key := map[string]string{"nan": "create_time", "inf": "create_time", "owner-nan": "owner_create_time"}[damage]
				state[key] = "\x00"
				data, _ = json.Marshal(state)
				data = []byte(lr4ReplaceFirst(string(data), `"\u0000"`, literal))
			case "list", "invalid":
				data = []byte(text)
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if damage == "unreadable" {
				_ = os.Remove(path)
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			got := lr4StateEndpoint()
			if damage == "valid" {
				if got["base_url"] != "http://127.0.0.1:59999/v1" || got["api_key"] != "test-only" {
					t.Fatalf("state endpoint = %v", got)
				}
				_ = proc.Kill()
				_, _ = proc.Wait()
				if got := lr4StateEndpoint(); got != nil {
					t.Fatalf("dead router endpoint = %v", got)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("the record must be retained")
				}
				return
			}
			if got != nil {
				t.Fatalf("state endpoint = %v, want none", got)
			}
			if damage != "list" && damage != "invalid" && damage != "unreadable" {
				if recordedProcess(readLocalRuntimeState()) != nil {
					t.Fatal("a damaged record resolved")
				}
			}
			if !real.IsRunning() {
				t.Fatal("the router was disturbed")
			}
		})
	}
}

// Replaces the first occurrence of old in s
func lr4ReplaceFirst(s, old, repl string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + repl + s[i+len(old):]
		}
	}
	return s
}
