package acp

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

type restoreStubAgent struct{ model string }

func TestAcpRestoreHealsAlternationForLiveReplay(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	db, err := state.Open(filepath.Join(t.TempDir(), "test_state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "acp1", Source: "acp"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range [][2]string{{"user", "first ask"}, {"assistant", "first reply"}, {"user", "unanswered turn"}, {"user", "next turn"}, {"assistant", "next reply"}} {
		if _, err := db.AppendMessage(ctx, "acp1", map[string]any{"role": row[0], "content": row[1]}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	mgr := NewSessionManager(func() any { return &restoreStubAgent{model: "stub"} }, db)
	st := mgr.restore(ctx, "acp1")
	if st == nil {
		t.Fatal("restore returned nil")
	}
	var roles []string
	for _, m := range st.History {
		roles = append(roles, m["role"].(string))
	}
	if !reflect.DeepEqual(roles, []string{"user", "assistant", "user", "assistant"}) {
		t.Fatalf("roles %v", roles)
	}
	for i := 1; i < len(roles); i++ {
		if roles[i-1] == "user" && roles[i] == "user" {
			t.Fatal("unhealed user;user in ACP live replay")
		}
	}
	merged, _ := st.History[2]["content"].(string)
	if !strings.Contains(merged, "unanswered turn") || !strings.Contains(merged, "next turn") {
		t.Fatalf("merged %q", merged)
	}
}
