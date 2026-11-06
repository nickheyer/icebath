package kanban

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Clears the board env pins every path priority case starts from
func cleanBoardEnv(t *testing.T) {
	for _, k := range []string{"ICEBATH_KANBAN_DB", "ICEBATH_KANBAN_BOARD", "ICEBATH_KANBAN_HOME", "ICEBATH_KANBAN_TASK", tools.DelegatedChildEnvMarker} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestExplicitBoardTrumpsEnvPins(t *testing.T) {
	cleanBoardEnv(t)
	tmp := t.TempDir()
	t.Setenv("ICEBATH_KANBAN_DB", filepath.Join(tmp, "pinned.db"))
	if p := mustDBPath(t, BoardArg("sycode-trading")); !strings.Contains(p, "sycode-trading") {
		t.Fatalf("explicit board should override env pin, got %s", p)
	}
	os.Unsetenv("ICEBATH_KANBAN_DB")
	t.Setenv("ICEBATH_KANBAN_BOARD", "jarvis-os")
	if p := mustDBPath(t, BoardArg("sycode-trading")); !strings.Contains(p, "sycode-trading") {
		t.Fatalf("explicit board should override ICEBATH_KANBAN_BOARD, got %s", p)
	}
}

func TestNoneBoardFallsBackToEnvPins(t *testing.T) {
	cleanBoardEnv(t)
	tmp := t.TempDir()
	pinned := filepath.Join(tmp, "pinned.db")
	t.Setenv("ICEBATH_KANBAN_DB", pinned)
	if p := mustDBPath(t, nil); p != pinned {
		t.Fatalf("None board with env pin should use env, got %s", p)
	}
	_ = mustDBPath(t, BoardArg("sycode-trading"))
	if p := mustDBPath(t, nil); p != pinned {
		t.Fatalf("env pin should survive after explicit-board call, got %s", p)
	}
	os.Unsetenv("ICEBATH_KANBAN_DB")
	kh := filepath.Join(tmp, "icebath_test")
	boardDir := filepath.Join(kh, "kanban", "boards", "jarvis-os")
	os.MkdirAll(boardDir, 0o755)
	os.WriteFile(filepath.Join(boardDir, "board.json"), []byte(`{"name": "Jarvis OS"}`), 0o644)
	t.Setenv("ICEBATH_KANBAN_HOME", kh)
	t.Setenv("ICEBATH_KANBAN_BOARD", "jarvis-os")
	if p := mustDBPath(t, nil); !strings.Contains(p, "jarvis-os") {
		t.Fatalf("None board should fall back to ICEBATH_KANBAN_BOARD, got %s", p)
	}
}

func TestExplicitBoardWorksWithoutAnyEnvAndForWorkspaces(t *testing.T) {
	cleanBoardEnv(t)
	if p := mustDBPath(t, BoardArg("sycode-trading")); !strings.Contains(p, "sycode-trading") {
		t.Fatal(p)
	}
	t.Setenv("ICEBATH_KANBAN_WORKSPACES_ROOT", filepath.Join(t.TempDir(), "pinned-workspaces"))
	p, err := WorkspacesRoot(context.Background(), BoardArg("sycode-trading"))
	if err != nil || !strings.Contains(p, "sycode-trading") {
		t.Fatal(p, err)
	}
}

func TestScopedCurrentBoardContextTrumpsKanbanDBEnv(t *testing.T) {
	cleanBoardEnv(t)
	t.Setenv("ICEBATH_KANBAN_DB", filepath.Join(t.TempDir(), "worker-pinned.db"))
	p, err := KanbanDBPath(ScopedCurrentBoard(context.Background(), "sycode-trading"), nil)
	if err != nil || !strings.Contains(p, "sycode-trading") {
		t.Fatalf("scoped --board context should override ICEBATH_KANBAN_DB, got %s %v", p, err)
	}
}

func TestDispatchedWorkerBoardArgStaysPinned(t *testing.T) {
	cleanBoardEnv(t)
	tmp := t.TempDir()
	pinned, pinnedWS := filepath.Join(tmp, "worker-pinned.db"), filepath.Join(tmp, "worker-pinned-workspaces")
	t.Setenv("ICEBATH_KANBAN_DB", pinned)
	t.Setenv("ICEBATH_KANBAN_WORKSPACES_ROOT", pinnedWS)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_3f1c63a5")
	if p := mustDBPath(t, BoardArg("other-board")); p != pinned {
		t.Fatal(p)
	}
	if p, _ := WorkspacesRoot(context.Background(), BoardArg("other-board")); p != pinnedWS {
		t.Fatal(p)
	}
}

func TestDelegatedChildBoardArgStaysPinned(t *testing.T) {
	cleanBoardEnv(t)
	tmp := t.TempDir()
	pinned := filepath.Join(tmp, "worker-pinned.db")
	t.Setenv("ICEBATH_KANBAN_DB", pinned)
	t.Setenv(tools.DelegatedChildEnvMarker, tmp)
	p := mustDBPath(t, BoardArg("other-board"))
	if p != pinned || !tools.KanbanPathIsFenced(context.Background(), p) {
		t.Fatal(p)
	}
	if _, err := WriteBoardMetadata(context.Background(), nil, BoardMetadataUpdate{}); err != nil {
		t.Fatal("metadata outside the fenced root refused", err)
	}
	t.Setenv("ICEBATH_KANBAN_HOME", tmp)
	var perr *PermissionError
	if _, err := WriteBoardMetadata(context.Background(), nil, BoardMetadataUpdate{}); !errors.As(err, &perr) {
		t.Fatalf("fenced metadata write allowed: %v", err)
	}
}

func TestPinFirstBoardResolutionMapsEverySlugToThePin(t *testing.T) {
	cleanBoardEnv(t)
	pinned := filepath.Join(t.TempDir(), "pinned.db")
	t.Setenv("ICEBATH_KANBAN_DB", pinned)
	ctx := PinFirstBoardResolution(context.Background())
	for _, slug := range []string{"default", "alpha", "beta"} {
		if p, err := KanbanDBPath(ctx, BoardArg(slug)); err != nil || p != pinned {
			t.Fatalf("%s -> %s %v", slug, p, err)
		}
	}
	os.Unsetenv("ICEBATH_KANBAN_DB")
	if p, _ := KanbanDBPath(ctx, BoardArg("alpha")); !strings.Contains(p, filepath.Join("boards", "alpha")) {
		t.Fatalf("without a pin the explicit board must win, got %s", p)
	}
}

func TestBoardMetadataRoundTripListAndRemove(t *testing.T) {
	cleanBoardEnv(t)
	setupKanbanHome(t)
	ctx := context.Background()
	if got := defaultBoardDisplayName("atm10-server"); got != "Atm10 Server" {
		t.Fatal(got)
	}
	name, desc := "  ", "notes"
	meta, err := CreateBoard(ctx, "Atm10_Server", BoardCreateOptions{Name: &name, Description: &desc})
	if err != nil {
		t.Fatal(err)
	}
	if objGet(meta, "slug") != "atm10_server" || objGet(meta, "name") != "Atm10 Server" || objGet(meta, "description") != "notes" ||
		objGet(meta, "archived") != false || objGet(meta, "created_at") == nil {
		t.Fatal(meta.Keys, meta.Vals)
	}
	if _, err := os.Stat(objGet(meta, "db_path").(string)); err != nil {
		t.Fatal("board database not created", err)
	}
	if err := os.WriteFile(filepath.Join(BoardsRoot(), "stub-only", "kanban.db"), nil, 0o644); err == nil {
		t.Fatal("stub dir unexpectedly present")
	}
	os.MkdirAll(filepath.Join(BoardsRoot(), "stub-only"), 0o755)
	os.WriteFile(filepath.Join(BoardsRoot(), "stub-only", "kanban.db"), nil, 0o644)
	boards, err := ListBoards(true)
	if err != nil || len(boards) != 2 || objGet(boards[0], "slug") != "default" || objGet(boards[1], "slug") != "atm10_server" {
		t.Fatal(len(boards), err)
	}
	if _, err := SetCurrentBoard(ctx, "atm10_server"); err != nil || GetCurrentBoard(ctx) != "atm10_server" {
		t.Fatal(err, GetCurrentBoard(ctx))
	}
	res, err := RemoveBoard(ctx, "atm10_server", true)
	if err != nil || objGet(res, "action") != "archived" || !pathExists(objGet(res, "new_path").(string)) {
		t.Fatal(res, err)
	}
	if GetCurrentBoard(ctx) != DefaultBoard {
		t.Fatal("current board not reverted")
	}
	tomb, _ := ReadBoardMetadata(BoardArg("atm10_server"))
	if objGet(tomb, "archived") != true || objGet(tomb, "name") != "Atm10 Server" {
		t.Fatal(tomb.Vals)
	}
	if active, _ := ListBoards(false); len(active) != 1 {
		t.Fatal(len(active))
	}
	var verr *ValueError
	if _, err := Connect(ctx, "", BoardArg("atm10_server")); !errors.As(err, &verr) || !strings.Contains(verr.Msg, "is archived") {
		t.Fatal(err)
	}
	if _, err := Connect(ctx, "", BoardArg("never-made")); !errors.As(err, &verr) || !strings.Contains(verr.Msg, "does not exist; create it with `icebath kanban boards create never-made`") {
		t.Fatal(err)
	}
	if _, err := RemoveBoard(ctx, "default", true); !errors.As(err, &verr) || verr.Msg != "the 'default' board cannot be removed" {
		t.Fatal(err)
	}
	if _, err := CreateBoard(ctx, "atm10_server", BoardCreateOptions{}); err != nil {
		t.Fatal("recreating an archived slug failed", err)
	}
	res, err = RemoveBoard(ctx, "atm10_server", false)
	if err != nil || objGet(res, "action") != "deleted" || pathExists(filepath.Join(BoardsRoot(), "atm10_server")) {
		t.Fatal(res, err)
	}
	if _, err := normalizeBoardSlug(BoardArg("-bad")); !errors.As(err, &verr) {
		t.Fatal(err)
	}
}

func TestNormalizeReasoningEffortAndSkills(t *testing.T) {
	high := " HIGH "
	if v, err := NormalizeReasoningEffort(&high); err != nil || *v != "high" {
		t.Fatal(v, err)
	}
	if v, err := NormalizeReasoningEffort(nil); err != nil || v != nil {
		t.Fatal(v, err)
	}
	bogus := "turbo"
	if _, err := NormalizeReasoningEffort(&bogus); err == nil || !strings.HasPrefix(err.Error(), "reasoning_effort must be one of none, ") || !strings.HasSuffix(err.Error(), ", got 'turbo'") {
		t.Fatal(err)
	}
	if got, err := normalizeTaskSkills([]string{" a ", "", "a", "b"}); err != nil || strings.Join(got, ",") != "a,b" {
		t.Fatal(got, err)
	}
	if got, _ := normalizeTaskSkills(nil); got != nil {
		t.Fatal(got)
	}
	if _, err := normalizeTaskSkills([]string{"a,b"}); err == nil || !strings.Contains(err.Error(), "skill name cannot contain comma: 'a,b'") {
		t.Fatal(err)
	}
	if m, p, err := validateModelOverride(nil, BoardArg("openrouter")); err == nil || m != nil || p != nil {
		t.Fatal("provider without model accepted")
	}
	if !strings.HasPrefix(newTaskID(), "t_") || len(newTaskID()) != 10 || !strings.HasSuffix(hostPrefix(), ":") {
		t.Fatal(newTaskID(), hostPrefix())
	}
}
