package pm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCorruptPackageStateCannotBeReplacedByAPartialWrite(t *testing.T) {
	isolateHome(t)
	path := filepath.Join(t.TempDir(), "facts.json")
	facts, err := NewFacts(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.RecordState("venv", "old", []string{"all"}, "", ""); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, []byte("not JSON"), 0o666)
	if err := facts.Reload(); err == nil || !strings.Contains(err.Error(), "recorded package state") {
		t.Fatalf("reload of corrupt state returned %v", err)
	}
	if err := facts.RecordState("venv", "new", []string{}, "", ""); err == nil || !strings.Contains(err.Error(), "recorded package state") {
		t.Fatalf("record over corrupt state returned %v", err)
	}
	if got := string(readFile(t, path)); got != "not JSON" {
		t.Fatalf("corrupt state rewritten to %q", got)
	}
}

// One writer child with its piped stdin, stdout lines and exit result
type sttWriter struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	stderr bytes.Buffer
	exited chan error
}

// Starts a lock writer child that snapshots the lockfile, pins its name and waits to save
func sttStartWriter(t *testing.T, path, name string) *sttWriter {
	t.Helper()
	w := &sttWriter{cmd: sttHelperCommand("lockwriter", path, name), lines: make(chan string, 4), exited: make(chan error, 1)}
	w.cmd.Stderr = &w.stderr
	stdin, err := w.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := w.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	w.stdin = stdin
	if err := w.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if w.cmd.ProcessState == nil {
			w.cmd.Process.Kill()
		}
	})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			w.lines <- scanner.Text()
		}
		close(w.lines)
		w.exited <- w.cmd.Wait()
	}()
	return w
}

func TestPinWritersMergeWithoutLosingOtherRows(t *testing.T) {
	isolateHome(t)
	path := filepath.Join(t.TempDir(), "lock.json")
	seed := NewLockfile(path)
	seed.SetPin("left", "1", map[string]any{})
	seed.SetPin("right", "1", map[string]any{})
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}
	var writers []*sttWriter
	for _, name := range []string{"left", "right"} {
		writers = append(writers, sttStartWriter(t, path, name))
	}
	for _, w := range writers {
		select {
		case line := <-w.lines:
			if line != "ready" {
				t.Fatalf("writer said %q: %s", line, w.stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("writer did not reach the snapshot barrier")
		}
	}
	for _, w := range writers {
		if _, err := io.WriteString(w.stdin, "save\n"); err != nil {
			t.Fatal(err)
		}
		w.stdin.Close()
	}
	for _, w := range writers {
		for range w.lines {
		}
		select {
		case err := <-w.exited:
			if err != nil {
				t.Fatalf("writer failed: %v %s", err, w.stderr.String())
			}
		case <-time.After(30 * time.Second):
			t.Fatal("writer never finished")
		}
	}
	final := NewLockfile(path)
	left, _ := final.Version("left")
	right, _ := final.Version("right")
	if left != "2" || right != "2" {
		t.Fatalf("left %q right %q", left, right)
	}
}

// Bytes of a file, nil when it does not exist
func sttBytesOrNil(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Modification time of a file in nanoseconds
func sttMtimeNS(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime().UnixNano()
}

func TestStaleOrInvalidEvidenceNeverGetsOverwritten(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "lock.json")
	initial := func() map[string]any {
		return map[string]any{"schema": 1, "packages": map[string]any{
			"tool": map[string]any{"version": "1", "artifacts": map[string]any{}},
			"keep": map[string]any{"version": "1", "artifacts": map[string]any{}},
		}}
	}
	writeJSON := func(v any) {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, data, 0o666)
	}
	for _, change := range []string{"changed", "deleted", "corrupt", "removed-file"} {
		writeJSON(initial())
		stale := NewLockfile(path)
		stale.SetPin("extra", "2", map[string]any{})
		stale.SetPin("tool", "2", map[string]any{})
		switch change {
		case "corrupt":
			writeFile(t, path, []byte("not JSON"), 0o666)
		case "removed-file":
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		default:
			var current map[string]any
			if err := json.Unmarshal(readFile(t, path), &current); err != nil {
				t.Fatal(err)
			}
			packages := current["packages"].(map[string]any)
			if change == "changed" {
				packages["tool"].(map[string]any)["version"] = "3"
			} else {
				delete(packages, "tool")
			}
			writeJSON(current)
		}
		before := sttBytesOrNil(t, path)
		if err := stale.Save(); err == nil {
			t.Fatalf("%s: stale save succeeded", change)
		}
		if after := sttBytesOrNil(t, path); !bytes.Equal(after, before) {
			t.Fatalf("%s: lockfile changed to %q", change, after)
		}
		if sttExists(filepath.Join(dir, "lock.corrupt")) {
			t.Fatalf("%s: strict read preserved a corrupt copy", change)
		}
	}
	writeJSON(initial())
	first, retry := NewLockfile(path), NewLockfile(path)
	first.SetPin("tool", "2", map[string]any{})
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)
	stamp := time.Unix(0, 1234567890)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	modified := sttMtimeNS(t, path)
	retry.SetPin("tool", "2", map[string]any{})
	if err := retry.Save(); err != nil {
		t.Fatal(err)
	}
	if sttMtimeNS(t, path) != modified {
		t.Fatal("an unchanged merge rewrote the lockfile")
	}
	if err := retry.Save(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, path), before) || sttMtimeNS(t, path) != modified {
		t.Fatal("a save with nothing touched rewrote the lockfile")
	}
}
