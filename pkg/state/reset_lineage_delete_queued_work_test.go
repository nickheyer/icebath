package state

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var resetLineageOperations = map[string]func(ctx context.Context, db *SessionDB, s0, s1 string) error{
	"single": func(ctx context.Context, db *SessionDB, _, s1 string) error {
		_, err := db.DeleteSession(ctx, s1, DeleteSessionOptions{ExcludeActiveWriteGuards: true})
		return err
	},
	"bulk": func(ctx context.Context, db *SessionDB, _, s1 string) error {
		_, err := db.DeleteSessions(ctx, []string{s1}, DeleteSessionsOptions{ExcludeActiveWriteGuards: true})
		return err
	},
	"never-active": func(ctx context.Context, db *SessionDB, _, _ string) error {
		_, _, _, err := db.PruneNeverActiveKeyedSessions(ctx, NeverActivePruneOptions{OlderThanDays: 30})
		return err
	},
	"prune-child": func(ctx context.Context, db *SessionDB, _, _ string) error {
		days, before := 30.0, now()-30*86400
		_, err := db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: &days, Filters: PruneFilters{StartedBefore: &before}})
		return err
	},
	"prune-all": func(ctx context.Context, db *SessionDB, _, _ string) error {
		before := now() + 60
		_, err := db.PruneSessions(ctx, PruneSessionsOptions{Filters: PruneFilters{LastActiveBefore: &before}})
		return err
	},
	"prune-empty": func(ctx context.Context, db *SessionDB, _, _ string) error {
		_, err := db.DeleteEmptySessions(ctx, "")
		return err
	},
}

// Local conversation reset from its creation id onto a child segment, aged, with an optional queued follower
func resetConversation(t *testing.T, operation string, queued bool) (*SessionDB, string, string, *pyjson.Object) {
	t.Helper()
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	s0 := seedLocalSession(t, db, t.TempDir())
	if _, err := db.AppendMessage(ctx, s0, map[string]any{"role": "user", "content": "root history"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	s1 := uuid4Hex()
	routed, err := routedEntry(ctx, db.conn, "local:"+s0, s0)
	if err != nil || routed == nil {
		t.Fatalf("route %v %v", routed, err)
	}
	entry := routed.Copy()
	entry.Set("session_id", s1)
	entry.Set("is_fresh_reset", true)
	if err := db.ResetLocalTarget(ctx, epoch, s0, entry); err != nil {
		t.Fatal(err)
	}
	old := now() - 200*86400
	profileExec(t, db, "UPDATE sessions SET started_at=? WHERE id IN (?,?)", old, s0, s1)
	if operation == "prune-child" || operation == "prune-all" || operation == "prune-empty" {
		profileExec(t, db, "UPDATE sessions SET ended_at=? WHERE id=?", old, s1)
	}
	var admission *pyjson.Object
	if queued {
		if admission, err = db.AdmitSessionInput(ctx, SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: s0, RequestID: "queued", Payload: admissionText("continue")}); err != nil {
			t.Fatal(err)
		}
	}
	return db, s0, s1, admission
}

// Which of the owner row, the segment row and the owner's policy receipt survive
func resetLineageState(t *testing.T, db *SessionDB, s0, s1 string) [3]bool {
	t.Helper()
	ctx := context.Background()
	owner, err := db.GetSession(ctx, s0)
	if err != nil {
		t.Fatal(err)
	}
	segment, err := db.GetSession(ctx, s1)
	if err != nil {
		t.Fatal(err)
	}
	policy := scalarOf(t, db.conn, "SELECT 1 FROM state_meta WHERE key=?", LocalPolicyPrefix+s0) != nil
	return [3]bool{owner != nil, segment != nil, policy}
}

func TestSegmentDeleteOrSweepKeepsAConversationWithQueuedWork(t *testing.T) {
	ctx := context.Background()
	names := make([]string, 0, len(resetLineageOperations))
	for name := range resetLineageOperations {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, operation := range names {
		t.Run(operation, func(t *testing.T) {
			db, s0, s1, admission := resetConversation(t, operation, true)
			defer db.Close()
			if operation == "never-active" {
				rows, err := db.ListNeverActiveKeyedSessions(ctx, 30)
				if err != nil || !slices.ContainsFunc(rows, func(r *pyjson.Object) bool { return rowValue(r, "id") == s1 }) {
					t.Fatalf("segment not listed never active %v %v", rows, err)
				}
			}
			if err := resetLineageOperations[operation](ctx, db, s0, s1); err != nil {
				var refusal *RuntimeStoreError
				if !errors.As(err, &refusal) || (operation != "single" && operation != "bulk") || refusal.Reason != "session_busy" {
					t.Fatalf("%s refused %v", operation, err)
				}
			}
			if got := resetLineageState(t, db, s0, s1); got != [3]bool{true, true, true} {
				t.Fatalf("%s left %v", operation, got)
			}
			row, err := db.GetSessionAdmission(ctx, rowValue(admission, "admission_id").(string))
			if err != nil || rowValue(row, "status") != "queued" {
				t.Fatalf("queued follower %v %v", row, err)
			}
		})
	}
}

func TestIdleConversationDeletesWholeFromItsCurrentSegment(t *testing.T) {
	ctx := context.Background()
	for _, operation := range []string{"single", "bulk", "prune-all"} {
		t.Run(operation, func(t *testing.T) {
			db, s0, s1, _ := resetConversation(t, operation, false)
			defer db.Close()
			if err := resetLineageOperations[operation](ctx, db, s0, s1); err != nil {
				t.Fatal(err)
			}
			if got := resetLineageState(t, db, s0, s1); got != [3]bool{false, false, false} {
				t.Fatalf("%s left %v", operation, got)
			}
		})
	}
}
