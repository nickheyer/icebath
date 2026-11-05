package profiles

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

const childInstallEnv = "ICEBATH_PROFILES_TEST_CHILD_INSTALL"

// Registers this process as a serve runtime of the install named by the environment and waits for stdin
func TestSharedWarningChildProcess(t *testing.T) {
	install := os.Getenv(childInstallEnv)
	if install == "" {
		return
	}
	if !tools.RegisterSelf("serve", install, nil) {
		fmt.Println("register failed")
		os.Exit(3)
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(0)
}

// Live child registered for install against homeDir, stopped at cleanup
func runningInstall(t *testing.T, homeDir, install string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSharedWarningChildProcess$")
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == tools.SpawnEnvVar || k == "ICEBATH_PARENT_PID" || k == "ICEBATH_PARENT_START_MARKER" || k == home.EnvHome {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, home.EnvHome+"="+homeDir, childInstallEnv+"="+install)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("child said %q", line)
	}
	t.Cleanup(func() {
		_, _ = io.WriteString(stdin, "exit\n")
		if err := cmd.Wait(); err != nil {
			t.Errorf("child exit %v", err)
		}
	})
	return cmd
}

// Temp root home with the user home beside it
func sharedHomes(t *testing.T) (string, string) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "home")
	_ = os.MkdirAll(root, 0o755)
	t.Setenv(home.EnvHome, root)
	t.Setenv("HOME", tmp)
	home.ResetKeyCache()
	return tmp, root
}

func TestWarningTracksLiveOtherInstallInSameHome(t *testing.T) {
	tmp, root := sharedHomes(t)
	ctx := context.Background()
	current, other := filepath.Join(tmp, "stable"), filepath.Join(tmp, "canary")
	if w := SharedProfileWarning(ctx, "", current); w != "" {
		t.Fatalf("idle %q", w)
	}
	t.Run("live", func(t *testing.T) {
		child := runningInstall(t, root, other)
		if w := SharedProfileWarning(ctx, "", current); w == "" || !strings.Contains(strings.ToLower(w), "profile") {
			t.Fatalf("live %q", w)
		}
		if w := SharedProfileWarning(ctx, "", other); w != "" {
			t.Fatalf("own install %q", w)
		}
		var found map[string]any
		for _, e := range tools.LedgerEntries(tools.LedgerOptions{ProjectRoot: other}) {
			if fmt.Sprint(e["pid"]) == fmt.Sprint(child.Process.Pid) {
				found = e
			}
		}
		if found == nil || found["icebath_home"] != home.Key(root) {
			t.Fatalf("entry %v", found)
		}
		if w := SharedProfileWarning(ctx, filepath.Join(root, "profiles", "work"), current); w != "" {
			t.Fatalf("other home %q", w)
		}
	})
	if _, err := os.Stat(filepath.Join(root, tools.LedgerFilename)); err != nil {
		t.Fatal("ledger missing")
	}
	if w := SharedProfileWarning(ctx, "", current); w != "" {
		t.Fatalf("stale %q", w)
	}
	work := filepath.Join(root, "profiles", "work")
	_ = os.MkdirAll(work, 0o755)
	t.Run("profile", func(t *testing.T) {
		runningInstall(t, work, other)
		if w := SharedProfileWarning(ctx, "", current); w != "" {
			t.Fatalf("root warned %q", w)
		}
		if w := SharedProfileWarning(ctx, work, current); w == "" {
			t.Fatal("profile not warned")
		}
	})
}

func TestWarningRejectsReusedOrUnverifiableProcessIdentity(t *testing.T) {
	tmp, root := sharedHomes(t)
	ctx := context.Background()
	current := filepath.Join(tmp, "stable")
	runningInstall(t, root, filepath.Join(tmp, "canary"))
	ledger := filepath.Join(root, tools.LedgerFilename)
	raw, _ := os.ReadFile(ledger)
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		t.Fatalf("ledger %s %v", raw, err)
	}
	write := func() {
		b, _ := json.Marshal(entries)
		_ = os.WriteFile(ledger, b, 0o644)
	}
	original := entries[0]["create_time"].(float64)
	if SharedProfileWarning(ctx, "", current) == "" {
		t.Fatal("no warning")
	}
	for _, bad := range []any{original - 60, original - 0.5, nil} {
		entries[0]["create_time"] = bad
		write()
		if w := SharedProfileWarning(ctx, "", current); w != "" {
			t.Fatalf("create_time %v warned", bad)
		}
	}
	entries[0]["create_time"] = original
	delete(entries[0], "icebath_home")
	write()
	if SharedProfileWarning(ctx, "", current) != "" {
		t.Fatal("homeless entry warned")
	}
	entries[0]["icebath_home"] = home.Key(filepath.Join(root, "profiles", "other"))
	write()
	if SharedProfileWarning(ctx, "", current) != "" {
		t.Fatal("other home warned")
	}
	entries[0]["icebath_home"] = home.Key(root)
	write()
	if SharedProfileWarning(ctx, "", current) == "" {
		t.Fatal("restored entry silent")
	}
	_ = os.WriteFile(ledger, []byte("{broken"), 0o644)
	if SharedProfileWarning(ctx, "", current) != "" {
		t.Fatal("corrupt ledger warned")
	}
	if _, err := os.Stat(ledger + ".corrupt"); err != nil {
		t.Fatal("corrupt ledger not parked")
	}
	write()
	if SharedProfileWarning(ctx, "", current) == "" {
		t.Fatal("rewritten ledger silent")
	}
	_ = os.Remove(ledger)
	if SharedProfileWarning(ctx, "", current) != "" {
		t.Fatal("missing ledger warned")
	}
	for _, p := range []string{ledger, filepath.Join(root, "config.yaml"), filepath.Join(root, "state.db")} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s created", p)
		}
	}
}
