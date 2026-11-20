package agent

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestFormatIterationProgressHidesUnboundedCeiling(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cases := []struct {
		max  any
		want string
	}{
		{int64(math.MaxInt64), "iteration 3"},
		{nil, "iteration 3"},
		{250, "iteration 3/250"},
	}
	for _, c := range cases {
		out, err := FormatIterationProgress(context.Background(), 3, c.max)
		if err != nil {
			t.Fatal(err)
		}
		if out != c.want || strings.Contains(out, strconv.FormatInt(math.MaxInt64, 10)) {
			t.Fatalf("max %v gave %q", c.max, out)
		}
	}
	if _, err := FormatIterationProgress(context.Background(), 3, math.Inf(1)); err == nil {
		t.Fatal("an infinite cap must raise like int(inf)")
	}
}

func TestBuildActivitySnapshotIncludesCompatAliases(t *testing.T) {
	at, now := 100.0, 110.0
	extra := pyjson.NewObject()
	extra.Set("api_call_count", 1)
	snap := BuildActivitySnapshot(&at, "starting API call #1", string(state.ActivityProvenanceUnknown), &now, extra)
	want := map[string]any{
		"last_activity_at":          100.0,
		"last_activity_description": "starting API call #1",
		"last_activity_provenance":  "unknown",
		"seconds_since_activity":    10.0,
		"last_activity_ts":          100.0,
		"last_activity_desc":        "starting API call #1",
		"description":               "starting API call #1",
		"api_call_count":            1,
	}
	for k, v := range want {
		if got, ok := snap.Get(k); !ok || got != v {
			t.Fatalf("%s = %v", k, got)
		}
	}
	for _, k := range []string{"phase", "last_progress_at"} {
		if _, ok := snap.Get(k); ok {
			t.Fatalf("unexpected key %s", k)
		}
	}
}

func TestTerminalCompressionProvenanceAndPersistWindow(t *testing.T) {
	if !IsTerminalCompressionProvenance("agent.compression_turnhold") || IsTerminalCompressionProvenance("agent.compression") {
		t.Fatal("terminal provenance set")
	}
	a := &AIAgent{sessionActivityLastPersistMono: 42}
	ResetSessionActivityPersistWindow(a)
	ResetSessionActivityPersistWindow(nil)
	if a.sessionActivityLastPersistMono != 0 {
		t.Fatal("persist window not reset")
	}
}
