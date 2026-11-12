package builtin

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	impeccableReplayEnv       = "ICEBATH_TEST_IMPECCABLE_REPLAY"
	impeccableReplayLogEnv    = "ICEBATH_TEST_IMPECCABLE_REPLAY_LOG"
	impeccableEngineRecording = "testdata/recorded/impeccable-hermes-stop-engine.json"
)

// Cache root the package cases materialize shared trees under, removed when the run ends
var builtinTestCache string

// One engine run the recorder captured, workspace and skill paths written as the WS and SKILL tokens
type replayCall struct {
	Argv   []string           `json:"argv"`
	Stdin  string             `json:"stdin"`
	Env    map[string]*string `json:"env"`
	Stdout string             `json:"stdout"`
	Stderr string             `json:"stderr"`
	Exit   int                `json:"exit"`
	Files  map[string]*string `json:"files"`
}

// Source built engine calls and the upstream hermes plugin results of the hermes stop oracle case
type replayRecording struct {
	Engine string       `json:"engine"`
	Calls  []replayCall `json:"calls"`
	Plugin struct {
		Result              string         `json:"result"`
		TransformToolResult string         `json:"transform_tool_result"`
		PreVerify           map[string]any `json:"pre_verify"`
	} `json:"plugin"`
}

func TestMain(m *testing.M) {
	if recording := os.Getenv(impeccableReplayEnv); recording != "" {
		os.Exit(replayImpeccableEngine(recording, os.Getenv(impeccableReplayLogEnv)))
	}
	cache, err := os.MkdirTemp("", "icebath-builtin-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	builtinTestCache = cache
	code := m.Run()
	os.RemoveAll(cache)
	os.Exit(code)
}

// Recorded engine calls and plugin results read from testdata
func loadImpeccableRecording(t *testing.T) replayRecording {
	t.Helper()
	raw, err := os.ReadFile(impeccableEngineRecording)
	if err != nil {
		t.Fatal(err)
	}
	var rec replayRecording
	if err := json.Unmarshal(raw, &rec); err != nil || len(rec.Calls) != 2 {
		t.Fatalf("recording %d calls %v", len(rec.Calls), err)
	}
	if pinned := "impeccable-engine " + strings.TrimSpace(readTestFile(t, filepath.Join(impeccableRepoRoot, "ENGINE_VERSION"))); rec.Engine != pinned {
		t.Fatalf("recorded with %q, the checkout pins %q", rec.Engine, pinned)
	}
	return rec
}

// Serves the recorded call taking these arguments, stdin and environment, logging its index or the refusal
func replayImpeccableEngine(path, log string) int {
	note := func(line string) {
		if f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(f, line)
			f.Close()
		}
	}
	refuse := func(msg string) int {
		note("refused " + msg)
		fmt.Fprintln(os.Stderr, msg)
		return 97
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return refuse(err.Error())
	}
	var rec replayRecording
	if err := json.Unmarshal(raw, &rec); err != nil {
		return refuse(err.Error())
	}
	ws, err := os.Getwd()
	if err != nil {
		return refuse(err.Error())
	}
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		return refuse(err.Error())
	}
	subst := strings.NewReplacer("<WS>", ws, "<SKILL>", os.Getenv("IMPECCABLE_SKILL_DIR")).Replace
	for i, call := range rec.Calls {
		if !slices.Equal(call.Argv, os.Args[1:]) || subst(call.Stdin) != string(stdin) {
			continue
		}
		for _, key := range sortedKeys(call.Env) {
			got, set := os.LookupEnv(key)
			if want := call.Env[key]; (want == nil && set) || (want != nil && (!set || got != subst(*want))) {
				return refuse(fmt.Sprintf("call %d with %s=%q set %v", i, key, got, set))
			}
		}
		for _, rel := range sortedKeys(call.Files) {
			target := filepath.Join(ws, filepath.FromSlash(subst(rel)))
			if call.Files[rel] == nil {
				if err := os.Remove(target); err != nil {
					return refuse(err.Error())
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return refuse(err.Error())
			}
			if err := os.WriteFile(target, []byte(subst(*call.Files[rel])), 0o644); err != nil {
				return refuse(err.Error())
			}
		}
		note("served " + strconv.Itoa(i))
		os.Stdout.WriteString(subst(call.Stdout))
		os.Stderr.WriteString(subst(call.Stderr))
		return call.Exit
	}
	return refuse(fmt.Sprintf("no recorded call takes argv %q and stdin %s", os.Args[1:], stdin))
}
