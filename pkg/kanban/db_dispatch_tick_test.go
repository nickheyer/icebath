package kanban

import (
	"bytes"
	"context"
	stdlog "log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestDispatchUnreadableAllowlistWarnsPerLaneTask(t *testing.T) {
	h := setupKanbanHome(t)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("kanban: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conn := mustConnect(t, "", nil)
	t.Cleanup(func() { conn.Close() })
	first := mustCreate(t, conn, CreateTaskOptions{Title: "one", Assignee: sp("sage")})
	second := mustCreate(t, conn, CreateTaskOptions{Title: "two", Assignee: sp("sage")})
	prevProbes := GatewayDispatchProbes
	t.Cleanup(func() { GatewayDispatchProbes = prevProbes })
	GatewayDispatchProbes = GatewayDispatchProbeSet{
		SampleMemory:     pyjson.NewObject,
		ClassifyPressure: func(any, any) string { return "ok" },
		Liveness:         func(context.Context, string) (any, bool) { return nil, false },
	}
	var buf bytes.Buffer
	prevOut, prevFlags := stdlog.Writer(), stdlog.Flags()
	stdlog.SetOutput(&buf)
	t.Cleanup(func() { stdlog.SetOutput(prevOut); stdlog.SetFlags(prevFlags) })
	o := DefaultDispatchOptions()
	o.DryRun = true
	result, err := DispatchOnce(context.Background(), conn, o)
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Clone(result.SkippedNonspawnable)
	slices.Sort(got)
	want := []string{first, second}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("skipped nonspawnable %v, want %v", got, want)
	}
	if n := strings.Count(buf.String(), "could not read kanban.dispatch_profiles"); n != 2 {
		t.Fatalf("unreadable config warned %d times, want once per lane task\n%s", n, buf.String())
	}
}
