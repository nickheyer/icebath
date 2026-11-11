package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestASkippedResumeRefreshWarnsUnlessTheSessionIsBusy(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	for _, failure := range []error{state.NewRuntimeStoreError("session_busy", nil, nil),
		state.NewRuntimeStoreError("launch_credentials_unavailable", nil, nil),
		&PyError{Kind: "OSError", Msg: "config.yaml unreadable"}} {
		noteSkippedRefresh("s1", failure)
	}
	type level struct{ level, reason string }
	var levels []level
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		message, _ := record["msg"].(string)
		if !strings.Contains(message, "keeps its stored policy") {
			continue
		}
		parts := strings.Split(message, ": ")
		levels = append(levels, level{record["level"].(string), parts[len(parts)-1]})
	}
	want := []level{{"INFO", "session_busy"}, {"WARN", "launch_credentials_unavailable"}, {"WARN", "OSError"}}
	if len(levels) != len(want) {
		t.Fatalf("levels %v", levels)
	}
	for i := range want {
		if levels[i] != want[i] {
			t.Fatalf("levels %v", levels)
		}
	}
}
