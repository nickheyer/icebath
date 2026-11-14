package pm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Project whose committed generation a child process activates and then holds until its stdin reads a line
const bootLeaseChild = "ICEBATH_PM_BOOT_LEASE_CHILD"

// Child mode that activates the project's dependencies, reports ready and keeps its lease until told to finish
func bootLeaseChildMain(project string) int {
	if err := ActivateDependencies(context.Background(), project); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	return 0
}

func TestBootstrapLeaseSurvivesSelectionChange(t *testing.T) {
	tmp := isolateHome(t)
	ctx := context.Background()
	repo := sttMkdir(t, filepath.Join(tmp, "repo"))
	t.Setenv(home.EnvHome, filepath.Join(tmp, "home"))
	state := InstallStateDir(ctx, repo)
	for _, name := range []string{"first", "second", "unused"} {
		venv := filepath.Join(state, "environments", name, "venv")
		writeFile(t, filepath.Join(venv, "pyvenv.cfg"), []byte("version = 3.11"), 0o666)
		site, err := SitePackages(venv)
		if err != nil {
			t.Fatal(err)
		}
		sttMkdir(t, site)
		sttTouch(t, filepath.Join(filepath.Dir(venv), ".lease-managed"))
	}
	legacy := sttMkdir(t, filepath.Join(state, "environments", "old-unleased"))
	choose := func(name string) string {
		environment := filepath.Join(state, "environments", name, "venv")
		data, _ := json.Marshal(map[string]any{"packages": map[string]any{"venv": map[string]any{"environment": environment}}})
		writeFile(t, EnvironmentRuntimeFactsPath(ctx, repo), data, 0o666)
		return environment
	}
	first := choose("first")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe)
	child.Env = append(os.Environ(), bootLeaseChild+"="+repo)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	child.Stderr = &stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	finish := func() error {
		if finished {
			return nil
		}
		finished = true
		_, _ = stdin.Write([]byte("done\n"))
		stdin.Close()
		done := make(chan error, 1)
		go func() { done <- child.Wait() }()
		select {
		case err := <-done:
			return err
		case <-time.After(15 * time.Second):
			_ = child.Process.Kill()
			return fmt.Errorf("child did not finish: %w", <-done)
		}
	}
	defer finish()
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("child said %q, stderr %s", line, stderr.String())
	}
	choose("second")
	if _, err := collectGenerations(ctx, repo, 0); err != nil {
		t.Fatal(err)
	}
	if !isDirectory(first) || pathExists(filepath.Join(state, "environments", "unused")) || !isDirectory(legacy) {
		t.Fatalf("first %v unused %v legacy %v", isDirectory(first), pathExists(filepath.Join(state, "environments", "unused")), isDirectory(legacy))
	}
	if err := finish(); err != nil {
		t.Fatalf("child exited %v, stderr %s", err, stderr.String())
	}
	if _, err := collectGenerations(ctx, repo, 0); err != nil {
		t.Fatal(err)
	}
	if pathExists(first) || !isDirectory(filepath.Join(state, "environments", "second")) || !isDirectory(legacy) {
		t.Fatalf("after exit first %v second %v legacy %v", pathExists(first), isDirectory(filepath.Join(state, "environments", "second")), isDirectory(legacy))
	}
}
