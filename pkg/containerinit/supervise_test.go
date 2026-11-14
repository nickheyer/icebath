//go:build linux

package containerinit

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Supervisor over a temp scan dir with a reaper loop standing in for the init's SIGCHLD handling
func startTestSupervisor(t *testing.T) *supervisor {
	t.Helper()
	dir := t.TempDir()
	r := newReaper()
	sv, err := newSupervisor(dir, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := sv.listen(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
				r.reap()
			}
		}
	}()
	t.Cleanup(func() {
		sv.stopAll(2 * time.Second)
		r.reap()
		close(done)
	})
	return sv
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func waitStatus(t *testing.T, dir string, ok func(svcStatus) bool) svcStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := readStatus(dir); err == nil && ok(st) {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := readStatus(dir)
	t.Fatalf("status of %s never matched, last %+v", dir, st)
	return st
}

func TestSupervisorStartsStopsAndRestartsAService(t *testing.T) {
	sv := startTestSupervisor(t)
	svc := filepath.Join(sv.dir, "worker")
	writeScript(t, filepath.Join(svc, "run"), "exec sleep 30")
	if err := writeControl(svscanControl(sv.dir), "a"); err != nil {
		t.Fatal(err)
	}
	first := waitStatus(t, svc, func(s svcStatus) bool { return s.Up && s.Pid > 0 && s.Ready })
	if !supervised(svc) {
		t.Fatal("control fifo not served")
	}
	if code := runSvc([]string{"-d", svc}); code != 0 {
		t.Fatalf("svc -d = %d", code)
	}
	waitStatus(t, svc, func(s svcStatus) bool { return !s.Up && !s.WantUp && !s.Finishing })
	if code := runSvc([]string{"-u", svc}); code != 0 {
		t.Fatalf("svc -u = %d", code)
	}
	second := waitStatus(t, svc, func(s svcStatus) bool { return s.Up && s.Pid > 0 })
	if second.Pid == first.Pid {
		t.Fatal("service not restarted")
	}
	if code := runSvc([]string{"-t", svc}); code != 0 {
		t.Fatalf("svc -t = %d", code)
	}
	waitStatus(t, svc, func(s svcStatus) bool { return s.Up && s.Pid != second.Pid && s.Pid > 0 })
}

func TestServiceWithDownFileStaysDownUntilAskedAndFinish125KeepsItDown(t *testing.T) {
	sv := startTestSupervisor(t)
	svc := filepath.Join(sv.dir, "gateway-x")
	writeScript(t, filepath.Join(svc, "run"), "exit 78")
	writeScript(t, filepath.Join(svc, "finish"), `[ "$1" = "78" ] && exit 125; exit 0`)
	os.WriteFile(filepath.Join(svc, "down"), nil, 0o644)
	sv.scan(false)
	waitStatus(t, svc, func(s svcStatus) bool { return !s.Up && !s.WantUp && !s.NormallyUp })
	writeControl(filepath.Join(svc, "supervise", "control"), "u")
	waitStatus(t, svc, func(s svcStatus) bool { return !s.Up && !s.WantUp && s.ExitCode == 78 && !s.Finishing })
}

func TestLoggerReceivesServiceOutputThroughThePipe(t *testing.T) {
	sv := startTestSupervisor(t)
	svc := filepath.Join(sv.dir, "talker")
	out := filepath.Join(t.TempDir(), "log.txt")
	writeScript(t, filepath.Join(svc, "run"), "echo hello-from-service; exec sleep 30")
	writeScript(t, filepath.Join(svc, "log", "run"), "exec cat > "+out)
	sv.scan(false)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if raw, _ := os.ReadFile(out); strings.Contains(string(raw), "hello-from-service") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("logger never received the service output")
}

func TestPruneRetiresServicesWhoseDirectoryVanished(t *testing.T) {
	sv := startTestSupervisor(t)
	svc := filepath.Join(sv.dir, "gone")
	writeScript(t, filepath.Join(svc, "run"), "exec sleep 30")
	sv.scan(false)
	st := waitStatus(t, svc, func(s svcStatus) bool { return s.Up })
	os.RemoveAll(svc)
	if code := runSvscanctl([]string{"-an", sv.dir}); code != 0 {
		t.Fatalf("svscanctl = %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sv.mu.Lock()
		_, still := sv.services["gone"]
		sv.mu.Unlock()
		if !still {
			if _, err := os.Stat("/proc/" + strconv.Itoa(st.Pid)); err == nil {
				raw, _ := os.ReadFile("/proc/" + strconv.Itoa(st.Pid) + "/stat")
				if !strings.Contains(string(raw), ") Z") {
					t.Fatal("retired service process still running")
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("pruned service still supervised")
}

func TestS6LogRotatesAndKeepsTheArchiveCount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	actions, err := parseLogScript([]string{"n2", "s64", "T", dir})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 1, 2, 3, 4, time.UTC)
	for i := 0; i < 12; i++ {
		if err := applyLogLine(actions, "line-number-"+strconv.Itoa(i), now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	archives := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "@") && strings.HasSuffix(e.Name(), ".s") {
			archives++
		}
	}
	if archives != 2 {
		t.Fatalf("archives = %d, want 2", archives)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "current"))
	if !strings.HasPrefix(string(raw), "2026-11-12 01:02:") || !strings.Contains(string(raw), "line-number-11\n") {
		t.Fatalf("current = %q", raw)
	}
}

func TestMainArgvRoutesCommandsToIcebathOrThePathProgram(t *testing.T) {
	exe := "/opt/icebath/icebath"
	cases := []struct {
		args []string
		want string
	}{
		{nil, exe},
		{[]string{"--version"}, exe + " --version"},
		{[]string{"gateway", "run"}, exe + " gateway run"},
		{[]string{"sh", "-c", "exit 3"}, "/sh -c exit 3"},
	}
	for _, c := range cases {
		got := strings.Join(mainArgv(exe, c.args), " ")
		if !strings.HasSuffix(got, c.want) {
			t.Fatalf("mainArgv(%v) = %q, want suffix %q", c.args, got, c.want)
		}
	}
}

func TestContainerEnvSnapshotRestoresAndUnsetsEmptyValues(t *testing.T) {
	dir := t.TempDir()
	if err := saveContainerEnv(dir, []string{"KEEP=1", "EMPTY=", "MULTI=a\nb"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(loadContainerEnv(dir, []string{"EMPTY=was-set", "OTHER=x"}), "|")
	for _, want := range []string{"KEEP=1", "MULTI=a\nb", "OTHER=x"} {
		if !strings.Contains(got, want) {
			t.Fatalf("env %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "EMPTY=") {
		t.Fatalf("empty snapshot value did not unset: %q", got)
	}
}

func TestRestartDelayBacksOffWhileRunsDieYoungAndResetsAfterAHealthyRun(t *testing.T) {
	s := &service{}
	var got []time.Duration
	for i := 0; i < 8; i++ {
		got = append(got, s.restartDelayLocked(10*time.Millisecond))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delays %v, want %v", got, want)
		}
	}
	if d := s.restartDelayLocked(restartHealthyAfter); d != 0 || s.backoff != 0 {
		t.Fatalf("healthy run delay %v backoff %v", d, s.backoff)
	}
	if d := s.restartDelayLocked(200 * time.Millisecond); d != time.Second {
		t.Fatalf("first young death after a healthy run waited %v", d)
	}
}

func TestProductBinaryIsTheIcebathBesideTheSeparateInit(t *testing.T) {
	if got := productBinaryFor("/opt/icebath/icebath-init"); got != "/opt/icebath/icebath" {
		t.Fatalf("init binary resolved to %q", got)
	}
	if got := productBinaryFor("/opt/icebath/icebath"); got != "/opt/icebath/icebath" {
		t.Fatalf("product binary resolved to %q", got)
	}
}
