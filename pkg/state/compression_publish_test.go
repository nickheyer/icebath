package state

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestCompressionLineagePublishAndRecoveryVerbs(t *testing.T) {
	ctx := context.Background()
	db := watermarkDB(t, 2)
	if child, _ := db.FindLiveCompressionChild(ctx, "sess1"); child != nil {
		t.Fatal("live parent has no compression child")
	}
	err := db.PublishCompressionChild(ctx, rotationPublish(nil, nil))
	var busy *CompressionSessionBusyError
	if !errors.As(err, &busy) || busy.Msg != "Compression lease lost before publication: sess1" {
		t.Fatalf("unleased publish %v", err)
	}
	if !db.TryAcquireCompressionLock("sess1", "rotator", 300) {
		t.Fatal("lock")
	}
	empty := rotationPublish(nil, nil)
	empty.Messages = nil
	if err := db.PublishCompressionChild(ctx, empty); err == nil || err.Error() != "Compression child handoff must not be empty" {
		t.Fatalf("empty handoff %v", err)
	}
	if err := db.PatchSessionModelConfig(ctx, "sess1", map[string]any{"prompt_refresh_epoch": 4, "other": 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.PublishCompressionChild(ctx, rotationPublish(nil, nil)); err != nil {
		t.Fatal(err)
	}
	child, err := db.FindLiveCompressionChild(ctx, "sess1")
	if err != nil || child == nil || rowValue(child, "id") != "child1" {
		t.Fatalf("live child %v %v", child, err)
	}
	if got := ParseModelConfig(rowValue(child, "model_config")); !reflect.DeepEqual(got, map[string]any{"prompt_refresh_epoch": int64(4)}) {
		t.Fatalf("carried conversation keys %v", got)
	}
	if lineage, _ := db.GetCompressionLineage(ctx, "sess1"); !reflect.DeepEqual(lineage, []string{"sess1", "child1"}) {
		t.Fatalf("lineage from root %v", lineage)
	}
	if lineage, _ := db.GetCompressionLineage(ctx, "child1"); !reflect.DeepEqual(lineage, []string{"sess1", "child1"}) {
		t.Fatalf("lineage from tip %v", lineage)
	}
	if lineage, _ := db.GetCompressionLineage(ctx, "ghost"); len(lineage) != 0 {
		t.Fatalf("missing lineage %v", lineage)
	}
	if ok, err := db.ReopenOrphanedCompressionSession(ctx, "sess1"); ok || err != nil {
		t.Fatalf("published parent reopened %v %v", ok, err)
	}
	orphan := watermarkDB(t, 2)
	if err := orphan.EndSession("sess1", "compression"); err != nil {
		t.Fatal(err)
	}
	if !orphan.TryAcquireCompressionLock("sess1", "holder", 300) {
		t.Fatal("orphan lock")
	}
	if ok, _ := orphan.ReopenOrphanedCompressionSession(ctx, "sess1"); ok {
		t.Fatal("a live lease must block recovery")
	}
	profileExec(t, orphan, "UPDATE compression_locks SET expires_at = 1 WHERE session_id = 'sess1'")
	if ok, err := orphan.ReopenOrphanedCompressionSession(ctx, "sess1"); !ok || err != nil {
		t.Fatalf("orphan recovery %v %v", ok, err)
	}
	if holder, _ := orphan.GetCompressionLockHolder("sess1"); holder != nil {
		t.Fatal("expired holder survived recovery")
	}
	closed := watermarkDB(t, 2)
	if err := closed.EndSession("sess1", "tui_close"); err != nil {
		t.Fatal(err)
	}
	reason, err := closed.ReopenIfExplicitlyClosed(ctx, "sess1", "routing", nil)
	if err != nil || reason == nil || *reason != "tui_close" {
		t.Fatalf("explicit close %v %v", reason, err)
	}
	if err := closed.EndSession("sess1", "tui_shutdown"); err != nil {
		t.Fatal(err)
	}
	if reason, _ := closed.ReopenIfExplicitlyClosed(ctx, "sess1", "routing", nil); reason != nil {
		t.Fatal("automatic stamps are left to publish")
	}
	stale := watermarkDB(t, 1)
	readSession(t, stale, NewSession{ID: "kid", Source: "cli", ParentSessionID: "sess1"})
	wmAppend(t, stale, "kid", map[string]any{"role": "user", "content": "x"}, AppendGuards{})
	if err := stale.EndSession("sess1", "compression"); err != nil {
		t.Fatal(err)
	}
	profileExec(t, stale, "UPDATE sessions SET started_at = 1 WHERE id = 'kid'")
	if n, err := stale.FinalizeOrphanedCompressionSessions(ctx); n != 1 || err != nil {
		t.Fatalf("finalized %d %v", n, err)
	}
	if info, _ := stale.GetSession(ctx, "kid"); rowValue(info, "end_reason") != "orphaned_compression" {
		t.Fatalf("orphan stamp %v", rowValue(info, "end_reason"))
	}
	if _, err := stale.StampLegacyCompressionChains(ctx); err != nil {
		t.Fatal(err)
	}
	if CreatedSurface("gui") != "desktop" || CreatedSurface("tool") != "tool" || CreatedSurface("local") != nil || CreatedSurface(nil) != nil || CreatedSurface("telegram") != "telegram" {
		t.Fatal("created surface mapping")
	}
}
