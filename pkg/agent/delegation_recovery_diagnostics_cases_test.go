package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	recoveryHandleEnv  = "ICEBATH_TEST_DELEGATION_RECOVERY_HANDLE"
	recoverySplitEnv   = "ICEBATH_TEST_DELEGATION_RECOVERY_SPLIT"
	recoveryMissingEnv = "ICEBATH_TEST_DELEGATION_RECOVERY_MISSING"
)

// Producer child of test_delegation_recovery_diagnostics.py, an owner that dispatches and dies before any child finishes
func dispatchDelegationRecoveryChild() {
	handlePath := os.Getenv(recoveryHandleEnv)
	if handlePath == "" {
		return
	}
	h := os.Getenv("ICEBATH_HOME")
	env := &tools.Env{Home: h, Config: config.Defaults(), Secrets: config.NewSecrets(h), Values: &sync.Map{}}
	if tools.AsyncDelegationRetirement == nil {
		tools.AsyncDelegationRetirement = reviewRetirementDouble{}
	}
	parent := &AIAgent{sessionID: "diagnostic-parent", env: env}
	started := make(chan struct{})
	var startOnce sync.Once
	buildChildPreservingParentToolsFn = func(context.Context, childBuildSpec) (*AIAgent, error) {
		return &AIAgent{delegateRole: "leaf", subagentID: "diagnostic-child"}, nil
	}
	runSingleChildFn = func(context.Context, int, string, *AIAgent, *AIAgent, string, any, any) *pyjson.Object {
		startOnce.Do(func() { close(started) })
		time.Sleep(60 * time.Second)
		return nil
	}
	resolveDelegationCredentialsFn = func(context.Context, map[string]any, *AIAgent) (map[string]any, error) {
		return map[string]any{"model": "m", "provider": nil, "base_url": nil, "api_key": nil, "api_mode": nil, "command": nil, "args": nil}, nil
	}
	if os.Getenv(recoveryMissingEnv) == "1" {
		original := createLiveTranscriptsFn
		createLiveTranscriptsFn = func(ctx context.Context, taskList []map[string]any, taskContext, model, providerName any, liveHome string) (string, []delegationLiveWriter, []string) {
			ident, writers, paths := original(ctx, taskList, taskContext, model, providerName, liveHome)
			writers[0] = nil
			return ident, writers, paths[1:]
		}
	}
	kwargs := map[string]any{"goal": "unfinished diagnostic task", "background": true}
	if os.Getenv(recoverySplitEnv) == "1" {
		kwargs = map[string]any{"tasks": []any{map[string]any{"goal": "first diagnostic task"}, map[string]any{"goal": "second diagnostic task"}}, "background": true}
	}
	out, err := delegateTask(tools.WithToolEnv(context.Background(), env), parent, kwargs)
	if err != nil {
		os.Stderr.WriteString("delegate_task: " + err.Error() + "\n")
		os.Exit(2)
	}
	handle, perr := pyjson.Loads(out)
	if perr != nil {
		os.Stderr.WriteString("handle: " + out + "\n")
		os.Exit(2)
	}
	if status, _ := handle.(*pyjson.Object).Get("status"); status != "dispatched" {
		os.Stderr.WriteString("handle: " + out + "\n")
		os.Exit(2)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		os.Stderr.WriteString("child did not start\n")
		os.Exit(2)
	}
	if err := os.WriteFile(handlePath, []byte(out), 0o600); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func stringMap(v any) map[string]string {
	out := map[string]string{}
	switch m := v.(type) {
	case map[string]string:
		for k, val := range m {
			out[k] = val
		}
	case map[string]any:
		for k, val := range m {
			out[k], _ = val.(string)
		}
	case *pyjson.Object:
		for _, k := range m.Keys {
			out[k], _ = m.Vals[k].(string)
		}
	}
	return out
}

// test_delegation_recovery_diagnostics.py test_unfinished_delegation_recovery_keeps_transcript_locator
func TestUnfinishedDelegationRecoveryKeepsTranscriptLocator(t *testing.T) {
	for _, tc := range []struct{ split, missingWriter bool }{{false, false}, {true, false}, {true, true}} {
		name := "single"
		if tc.split {
			name = "split"
		}
		if tc.missingWriter {
			name += "-missing-writer"
		}
		t.Run(name, func(t *testing.T) {
			env := delegationTestEnv(t, 0, false)
			if err := os.WriteFile(filepath.Join(env.Home, "config.yaml"), []byte("delegation:\n  independent_completions: true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			owner := filepath.Join(t.TempDir(), "owner")
			if err := os.MkdirAll(owner, 0o755); err != nil {
				t.Fatal(err)
			}
			runGit(t, owner, "init", "-q")
			runGit(t, owner, "commit", "-q", "--allow-empty", "-m", "init")
			if err := os.WriteFile(filepath.Join(owner, "dirty.txt"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			handlePath := filepath.Join(t.TempDir(), "dispatch.json")
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			flag := func(b bool) string {
				if b {
					return "1"
				}
				return "0"
			}
			producer := exec.Command(exe, "-test.run=^$")
			producer.Dir = owner
			producer.Env = append(os.Environ(), "ICEBATH_HOME="+env.Home, recoveryHandleEnv+"="+handlePath,
				recoverySplitEnv+"="+flag(tc.split), recoveryMissingEnv+"="+flag(tc.missingWriter))
			if out, err := producer.CombinedOutput(); err != nil {
				t.Fatalf("producer: %v\n%s", err, out)
			}
			raw, err := os.ReadFile(handlePath)
			if err != nil {
				t.Fatal(err)
			}
			handle := dispatchPayload(t, string(raw), nil)
			rawTranscripts, _ := handle.Get("live_transcripts")
			var transcripts []string
			for _, p := range rawTranscripts.([]any) {
				path := p.(string)
				if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
					t.Fatalf("transcript %q missing %v", path, err)
				}
				transcripts = append(transcripts, path)
			}
			q := tools.NewEventQueue()
			count, err := tools.RestoreUndeliveredCompletions(env, q)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if tc.split {
				want = 2
			}
			if count != want {
				t.Fatalf("restored %d events", count)
			}
			byIndex := map[string]string{}
			for i := 0; i < count; i++ {
				event, ok := q.GetNowait()
				if !ok {
					t.Fatalf("event %d missing", i)
				}
				message := tools.FormatProcessNotification(event)
				if event["status"] != "unknown" || event["last_known_status"] != "running" || !strings.Contains(message, "running") {
					t.Fatalf("event %v message %q", event, message)
				}
				if summary, _ := event["summary"].(string); summary != "" {
					t.Fatalf("event carries a summary %v", event)
				}
				paths := stringMap(event["task_transcripts"])
				if len(paths) > 1 {
					t.Fatalf("task_transcripts %v", paths)
				}
				for index, path := range paths {
					if !strings.Contains(message, path) || filepath.Base(path) != "task-"+index+".log" {
						t.Fatalf("index %s path %q message %q", index, path, message)
					}
					if _, dup := byIndex[index]; dup {
						t.Fatalf("index %s restored twice", index)
					}
					byIndex[index] = path
				}
				for _, path := range transcripts {
					inPaths := false
					for _, p := range paths {
						inPaths = inPaths || p == path
					}
					if strings.Contains(message, path) != inPaths {
						t.Fatalf("message %q names %q unexpectedly", message, path)
					}
				}
				tails := stringMap(event["transcript_tails"])
				if len(tails) != len(paths) {
					t.Fatalf("tails %v paths %v raw %#v event %v", tails, paths, event["transcript_tails"], event)
				}
				for index, tail := range tails {
					if _, ok := paths[index]; !ok || !strings.Contains(tail, "kickoff") || !strings.Contains(message, tail) {
						t.Fatalf("tail %s %q message %q", index, tail, message)
					}
				}
				hint, _ := event["git_state_hint"].(string)
				if !strings.Contains(hint, "uncommitted file(s)") || !strings.Contains(message, hint) {
					t.Fatalf("git hint %q message %q", hint, message)
				}
			}
			wantIndexes := map[string]bool{"0": true}
			if tc.missingWriter {
				wantIndexes = map[string]bool{"1": true}
			} else if tc.split {
				wantIndexes = map[string]bool{"0": true, "1": true}
			}
			if len(byIndex) != len(wantIndexes) {
				t.Fatalf("indexes %v want %v", byIndex, wantIndexes)
			}
			seen := map[string]bool{}
			for index, path := range byIndex {
				if !wantIndexes[index] {
					t.Fatalf("indexes %v want %v", byIndex, wantIndexes)
				}
				seen[path] = true
			}
			if len(seen) != len(transcripts) {
				t.Fatalf("paths %v transcripts %v", seen, transcripts)
			}
			for _, path := range transcripts {
				if !seen[path] {
					t.Fatalf("transcript %q not restored", path)
				}
			}
		})
	}
}
