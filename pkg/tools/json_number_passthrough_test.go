package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestOrderedArgsObjectKeepsTheModelIntsAndFloats(t *testing.T) {
	raw := `{"b": 1.0, "a": 1, "n": {"z": 2, "y": 2.5}}`
	args, parseErr := ParseToolArguments(raw)
	if parseErr != "" {
		t.Fatal(parseErr)
	}
	if got := dumpsUnicode(orderedArgsObject(raw, args)); got != `{"b": 1.0, "a": 1, "n": {"z": 2, "y": 2.5}}` {
		t.Fatalf("display %s", got)
	}
	args["a"] = 2.0
	if got := dumpsUnicode(orderedArgsObject(raw, args)); got != `{"b": 1.0, "a": 2.0, "n": {"z": 2, "y": 2.5}}` {
		t.Fatalf("changed key %s", got)
	}
}

func TestGuardrailCanonicalArgsAndResultHashKeepIntsAndFloats(t *testing.T) {
	raw := `{"ratio": 1.0, "limit": 20, "nested": {"b": 2, "a": 2.0}}`
	args, parseErr := ParseToolArguments(raw)
	if parseErr != "" {
		t.Fatal(parseErr)
	}
	if got := CanonicalToolArgs(orderedArgsObject(raw, args).Vals); got != `{"limit":20,"nested":{"a":2.0,"b":2},"ratio":1.0}` {
		t.Fatalf("canonical args %s", got)
	}
	result := `{"duration_seconds": 1.5, "kernel": {"execution_count": 3, "x": 1}, "out": 1.0}`
	if got := guardResultHash(&result, "execute_code"); got != "c1f287f1c3db24f9be494912e9c2ac14eb3e8ad6c70ff6b1fc8f3cae1a76a91d" {
		t.Fatalf("result hash %s", got)
	}
	intResult, floatResult := `{"n": 1}`, `{"n": 1.0}`
	if guardResultHash(&intResult, "read_file") == guardResultHash(&floatResult, "read_file") {
		t.Fatal("int and float results hashed alike")
	}
}

func TestReadPanePassesDesktopJSONThroughAsPythonDoes(t *testing.T) {
	answer := `{"total_lines": 3, "start": 0, "ratio": 1.0, "text": "a\u00e9"}`
	pane := func(window map[string]int) (string, error) { return answer, nil }
	if got := readPane(pane, nil, [4]string{}); got != `{"total_lines": 3, "start": 0, "ratio": 1.0, "text": "aé"}` {
		t.Fatalf("passthrough %s", got)
	}
	answer = "not json"
	if got := readPane(pane, nil, [4]string{}); got != `{"text": "not json"}` {
		t.Fatalf("wrapped %s", got)
	}
}

func TestPromotedNoteKeepsTheSpawnResultNumbers(t *testing.T) {
	spawn := `{"output": "", "session_id": "proc_1", "pid": 1234, "ratio": 1.0, "notify_on_complete": false}`
	note := strings.ReplaceAll(strings.ReplaceAll(promotedNotePollOnly, "{requested}", "900"), "{cap}", strconv.Itoa(foregroundMaxTimeout()))
	want := `{"output": "", "session_id": "proc_1", "pid": 1234, "ratio": 1.0, "notify_on_complete": false, "promoted_from_foreground": ` + pyDumps(note, pyjson.EnsureASCII(false)) + `}`
	if got := withPromotedNote(spawn, 900); got != want {
		t.Fatalf("promoted %s", got)
	}
	if failed := `{"error": "boom", "pid": 1}`; withPromotedNote(failed, 900) != failed {
		t.Fatal("error result rewritten")
	}
}

func TestExtractCacheIndexRewritesNumbersAsPythonDoes(t *testing.T) {
	env := &Env{Home: t.TempDir()}
	dir := extractCacheDir(env)
	path := filepath.Join(dir, extractIndexFilename)
	if err := os.WriteFile(path, []byte("\ufeff"+`{"d2": {"url": "https://b", "fetched_at": 1.0, "n": 1}, "d1": {"url": "https://a", "fetched_at": 1700000000.5}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	saveExtractIndex(context.Background(), env, loadExtractIndex(env))
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"d2": {"url": "https://b", "fetched_at": 1.0, "n": 1}, "d1": {"url": "https://a", "fetched_at": 1700000000.5}}` {
		t.Fatalf("index %s", got)
	}
}
