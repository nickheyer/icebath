//go:build windows

package updaterecord

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Windows stand-ins of the restart debt cases, a plain reader holding a file and the updater that paused
func debtChild(mode string, args []string) (int, bool) {
	ctx := context.Background()
	switch mode {
	case "debt_hold":
		name, err := windows.UTF16PtrFromString(args[0])
		if err != nil {
			panic(err)
		}
		handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			panic(err)
		}
		defer windows.CloseHandle(handle)
		pcPrint("held")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0, true
	case "debt_updater":
		token := map[string]any{"pause_id": args[0], "resume_needed": true, "profiles": map[string]any{"default": 4242}}
		if err := WritePause(ctx, token, PauseIdentity(0), ""); err != nil {
			panic(err)
		}
		pcPrint("written")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		if err := DischargePause(ctx, token, ""); err != nil {
			panic(err)
		}
		pcPrint("discharged")
		return 0, true
	}
	return 0, false
}

type debtProc struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func debtStart(t *testing.T, mode string, args ...string) *debtProc {
	t.Helper()
	cmd := pcCommand(mode, args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return &debtProc{cmd: cmd, in: in, out: bufio.NewReader(out)}
}

func (p *debtProc) line() string {
	line, _ := p.out.ReadString('\n')
	return strings.TrimSpace(line)
}

// Another process reading path with a plain open that shares read and write but not delete
func debtHold(t *testing.T, path string) *debtProc {
	t.Helper()
	proc := debtStart(t, "debt_hold", path)
	if got := proc.line(); got != "held" {
		t.Fatalf("holder said %q", got)
	}
	return proc
}

func debtRelease(t *testing.T, proc *debtProc) {
	t.Helper()
	_ = proc.in.Close()
	done := make(chan struct{})
	go func() {
		_ = proc.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the holder did not exit")
	}
}

func debtRecordFiles(t *testing.T, home string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(home, globEscape(PauseRecordStem)+"*"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, p := range paths {
		if ext := filepath.Ext(p); ext == ".json" || ext == ".claim" {
			names = append(names, filepath.Base(p))
		}
	}
	sort.Strings(names)
	return names
}

func debtOrphan(t *testing.T, profiles map[string]any) {
	t.Helper()
	var id [16]byte
	_, _ = rand.Read(id[:])
	token := map[string]any{"pause_id": hex.EncodeToString(id[:]), "resume_needed": true, "profiles": profiles}
	if err := WritePause(context.Background(), token, PauseUnowned(), ""); err != nil {
		t.Fatal(err)
	}
}

func debtOrphans(t *testing.T) []PauseFile {
	t.Helper()
	found, err := PauseOrphans(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestACompletedObligationNeverRunsAgainFromACopyWindowsKept(t *testing.T) {
	for _, carrier := range []string{"claim", "record"} {
		t.Run(carrier, func(t *testing.T) {
			ctx := context.Background()
			home := psHome(t)
			src := PauseRecordPath()
			if carrier == "claim" {
				debtOrphan(t, map[string]any{"default": 4242, "work": 4343})
				holder := debtHold(t, src)
				won, body, ok, err := ClaimPause(ctx, src)
				if err != nil || !ok || !pathExists(src) {
					debtRelease(t, holder)
					t.Fatalf("premise: Windows refused to delete the claimed source (%v %v)", ok, err)
				}
				token := shallowCopy(pauseTokenOf(body))
				token["resume_needed"], token["profiles"] = false, map[string]any{}
				handBack(ctx, won, body, token, map[string]any{"profiles": map[string]any{}, "unmapped": []any{}})
				found := debtOrphans(t)
				debtRelease(t, holder)
				if len(found) != 0 {
					t.Fatalf("the completed set is owed again from the copy Windows kept: %v", found)
				}
			} else {
				var id [16]byte
				_, _ = rand.Read(id[:])
				updater := debtStart(t, "debt_updater", hex.EncodeToString(id[:]))
				if got := updater.line(); got != "written" {
					t.Fatalf("updater said %q", got)
				}
				holder := debtHold(t, src)
				_, _ = io.WriteString(updater.in, "go\n")
				_ = updater.in.Close()
				out, _ := io.ReadAll(updater.out)
				_ = updater.cmd.Wait()
				if !strings.Contains(string(out), "discharged") || !pathExists(src) {
					debtRelease(t, holder)
					t.Fatalf("premise: Windows refused to delete the discharged record: %q", out)
				}
				found := debtOrphans(t)
				debtRelease(t, holder)
				if len(found) != 0 {
					t.Fatalf("a dead updater's discharged record is owed again: %v", found)
				}
			}
			if err := RetirePauseRedundant(ctx, ""); err != nil {
				t.Fatal(err)
			}
			if files := debtRecordFiles(t, home); len(files) != 0 {
				t.Fatalf("record files left %v", files)
			}
			if found := debtOrphans(t); len(found) != 0 {
				t.Fatalf("orphans left %v", found)
			}
			if pathExists(pauseRetiredPath(src)) {
				t.Fatal("the retired list outlived every copy it guarded")
			}
		})
	}
}

func TestAPartlyResumedClaimIsWhatTheNextLaunchResumesNotTheCopyWindowsKept(t *testing.T) {
	ctx := context.Background()
	psHome(t)
	src := PauseRecordPath()
	debtOrphan(t, map[string]any{"default": 4242, "work": 4343})
	holder := debtHold(t, src)
	won, body, ok, err := ClaimPause(ctx, src)
	if err != nil || !ok || !pathExists(src) {
		debtRelease(t, holder)
		t.Fatalf("premise: Windows refused to delete the claimed source (%v %v)", ok, err)
	}
	token := shallowCopy(pauseTokenOf(body))
	token["profiles"] = map[string]any{"work": 4343}
	handBack(ctx, won, body, token, map[string]any{"profiles": map[string]any{}, "unmapped": []any{}})
	found, err := PauseOrphans(ctx, "")
	debtRelease(t, holder)
	if err != nil {
		t.Fatal(err)
	}
	owed := []any{}
	for _, f := range found {
		owed = append(owed, pauseTokenOf(f.Body)["profiles"])
	}
	if !pyrepr.Equal(owed, []any{map[string]any{"work": 4343}}) {
		t.Fatalf("the next launch would restart a gateway this launch already restarted: %v", owed)
	}
}
