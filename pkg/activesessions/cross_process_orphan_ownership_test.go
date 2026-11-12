package activesessions

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const leaseHolderEnv = "ICEBATH_SESSION_LEASE_HOLDER"

// Waits for a signal file, exiting the child when it never appears
func awaitSignalFile(path, what string) {
	deadline := time.Now().Add(120 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			os.Stderr.WriteString("timed out waiting for " + what + "\n")
			os.Exit(1)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Child side of the lease contender, acquiring on the go file and holding until the release file
func TestHelperSessionLeaseHolder(t *testing.T) {
	if os.Getenv(leaseHolderEnv) != "1" {
		return
	}
	fail := func(msg string) { os.Stderr.WriteString(msg + "\n"); os.Exit(1) }
	if err := os.WriteFile(os.Getenv("WAITING_FILE"), []byte("waiting"), 0o644); err != nil {
		fail(err.Error())
	}
	awaitSignalFile(os.Getenv("GO_FILE"), "acquisition signal")
	if err := os.WriteFile(os.Getenv("BOUNDARY_FILE"), []byte("boundary"), 0o644); err != nil {
		fail(err.Error())
	}
	lease, refusal, err := TryAcquire(context.Background(), os.Getenv("SESSION_ID"), "desktop", map[string]any{}, nil, "", true)
	if err != nil || lease == nil || refusal != nil {
		fail("acquire failed")
	}
	if err := os.WriteFile(os.Getenv("READY_FILE"), []byte("ready"), 0o644); err != nil {
		fail(err.Error())
	}
	awaitSignalFile(os.Getenv("RELEASE_FILE"), "release signal")
	if err := lease.Release(); err != nil {
		fail(err.Error())
	}
	os.Exit(0)
}

func TestUnlimitedSessionLeaseIsRealEvenWithoutACap(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	registryHome := filepath.Join(t.TempDir(), "untracked-home")
	lease, refusal, err := TryAcquire(ctx, "untracked-session", "tui", map[string]any{}, nil, registryHome, false)
	if err != nil || lease == nil || refusal != nil {
		t.Fatalf("acquire %v %v %v", lease, refusal, err)
	}
	if !lease.Enabled {
		t.Fatal("lease is disabled")
	}
	entries, err := RegistrySnapshot(ctx, registryHome, false)
	if err != nil || len(entries) != 1 || entries[0].GetOr("session_id", nil) != "untracked-session" {
		t.Fatalf("registry %v %v", entries, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if entries, err := RegistrySnapshot(ctx, registryHome, false); err != nil || len(entries) != 0 {
		t.Fatalf("registry after release %v %v", entries, err)
	}
}

func TestLivenessGuardSerializesCrossProcessAcquire(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	tmp := t.TempDir()
	registryHome := filepath.Join(tmp, "guard-home")
	waiting, boundary, goFile := filepath.Join(tmp, "child-waiting"), filepath.Join(tmp, "child-lock-boundary"), filepath.Join(tmp, "child-go")
	acquired, release := filepath.Join(tmp, "child-acquired"), filepath.Join(tmp, "child-release")
	sessionID := "guarded-session"
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "-test.run=^TestHelperSessionLeaseHolder$")
	child.Env = append(os.Environ(), leaseHolderEnv+"=1", "ICEBATH_HOME="+registryHome, "SESSION_ID="+sessionID, "READY_FILE="+acquired,
		"RELEASE_FILE="+release, "BOUNDARY_FILE="+boundary, "GO_FILE="+goFile, "WAITING_FILE="+waiting)
	var out bytes.Buffer
	child.Stdout, child.Stderr = &out, &out
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	exited := false
	var exitErr error
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release"), 0o644)
		if !exited {
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				_ = child.Process.Kill()
				<-done
			}
		}
	})
	waitFile := func(path, label string) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for {
			if _, err := os.Stat(path); err == nil {
				return
			}
			select {
			case exitErr = <-done:
				exited = true
				t.Fatalf("child exited before %s: %v\n%s", label, exitErr, out.String())
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", label)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFile(waiting, "lease contender bootstrap")
	err = LivenessGuard(ctx, sessionID, registryHome, nil, func(active bool) error {
		if active {
			t.Error("guard saw an active lease")
		}
		if err := os.WriteFile(goFile, []byte("go"), 0o644); err != nil {
			t.Error(err)
		}
		waitFile(boundary, "lease lock boundary")
		time.Sleep(250 * time.Millisecond)
		if _, err := os.Stat(acquired); err == nil {
			t.Error("child acquired inside the guard")
		}
		select {
		case exitErr = <-done:
			exited = true
			t.Errorf("child exited inside the guard: %v", exitErr)
		default:
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFile(acquired, "lease contender")
	if err := os.WriteFile(release, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case exitErr = <-done:
		exited = true
	case <-time.After(30 * time.Second):
		t.Fatal("child did not exit after release")
	}
	if exitErr != nil {
		t.Fatalf("child: %v\n%s", exitErr, out.String())
	}
	if entries, err := RegistrySnapshot(ctx, registryHome, false); err != nil || len(entries) != 0 {
		t.Fatalf("registry after release %v %v", entries, err)
	}
}
