package proc

import (
	"errors"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"testing"
	"time"
)

const helperEnv = "ICEBATH_PROC_IDENTITY_HELPER"

func TestHelperProcessSleeps(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

// Starts this test binary as a sleeping child with a known argv and environment
func startHelper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessSleeps$", "with space")
	cmd.Env = append(os.Environ(), helperEnv+"=1", "ICEBATH_PROC_PROBE=marker value")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	return cmd
}

// Polls until the child has exec'd its own image and exposes the requested argv
func waitForArgv(t *testing.T, pid int, want []string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := Cmdline(pid)
		if err == nil && reflect.DeepEqual(got, want) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("cmdline of %d = %q, %v, want %q", pid, got, err, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPPIDOfSelfMatchesTheRuntime(t *testing.T) {
	ppid, ok := PPID(os.Getpid())
	if !ok || ppid != os.Getppid() {
		t.Fatalf("PPID = %d, %v, want %d", ppid, ok, os.Getppid())
	}
}

func TestChildProbesReadItsArgvEnvironmentBirthAndAncestry(t *testing.T) {
	cmd := startHelper(t)
	pid := cmd.Process.Pid
	waitForArgv(t, pid, cmd.Args)
	if ppid, ok := PPID(pid); !ok || ppid != os.Getpid() {
		t.Fatalf("child PPID = %d, %v", ppid, ok)
	}
	self, err := Birth(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	child, err := Birth(pid)
	if err != nil {
		t.Fatal(err)
	}
	if child < self || child > float64(time.Now().Unix())+2 {
		t.Fatalf("child birth %v outside [%v, now]", child, self)
	}
	env, err := Environ(pid)
	if err != nil {
		t.Fatal(err)
	}
	key := "ICEBATH_PROC_PROBE"
	if env[key] != "marker value" {
		t.Fatalf("environ[%s] = %q", key, env[key])
	}
	parents, err := Parents(pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) < 2 || parents[0] != os.Getpid() || parents[1] != os.Getppid() {
		t.Fatalf("parents of child = %v, self %d parent %d", parents, os.Getpid(), os.Getppid())
	}
	low, err := lowestPID()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(parents[:len(parents)-1], low) {
		t.Fatalf("walk continued past the lowest pid %d: %v", low, parents)
	}
}

func TestProbesOfAReapedChildReportNoSuchProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if _, err := Birth(pid); !errors.Is(err, ErrNoSuchProcess) {
		t.Fatalf("Birth of reaped pid err = %v", err)
	}
	if _, err := Cmdline(pid); !errors.Is(err, ErrNoSuchProcess) {
		t.Fatalf("Cmdline of reaped pid err = %v", err)
	}
	if _, ok := PPID(pid); ok {
		t.Fatal("PPID of reaped pid reported a parent")
	}
}

func TestParseEnvironBlockFollowsPsutil(t *testing.T) {
	cases := []struct {
		block string
		want  map[string]string
	}{
		{"A=1\x00B=x=y\x00\x00C=3\x00", map[string]string{"A": "1", "B": "x=y"}},
		{"=hidden\x00noequals\x00K=v\x00", map[string]string{"K": "v"}},
		{"K=v\x00tail=unterminated", map[string]string{"K": "v"}},
		{"\x00K=v\x00", map[string]string{}},
		{"", map[string]string{}},
	}
	for _, c := range cases {
		got := parseEnvironBlock(c.block)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseEnvironBlock(%q) = %v, want %v", c.block, got, c.want)
		}
	}
}
