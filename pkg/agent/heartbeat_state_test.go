package agent

import (
	"context"
	"testing"
)

func TestHeartbeatModule(t *testing.T) {
	t.Run("test_state_roundtrip", func(t *testing.T) {
		s := NewHeartbeatState("check CI", 600)
		s.CreatedAt = nowSeconds()
		raw, err := s.ToJSON()
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := HeartbeatStateFromJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Prompt != "check CI" || loaded.IntervalSeconds != 600 || loaded.Status != "active" {
			t.Fatalf("loaded %#v", loaded)
		}
	})
	t.Run("test_migrate_heartbeat_to_session", func(t *testing.T) {
		goalsTestHome(t)
		ctx := context.Background()
		s := NewHeartbeatState("carry me", 600)
		s.CreatedAt = nowSeconds()
		SaveHeartbeat(ctx, "hb-parent-sid", s)
		if !migrateHeartbeatToSession(ctx, "hb-parent-sid", "hb-child-sid") {
			t.Fatal("migration refused")
		}
		if child := LoadHeartbeat(ctx, "hb-child-sid"); child == nil || child.Prompt != "carry me" {
			t.Fatalf("child %#v", child)
		}
		if parent := LoadHeartbeat(ctx, "hb-parent-sid"); parent != nil {
			t.Fatalf("parent %#v", parent)
		}
	})
	t.Run("test_migrate_noop_without_source", func(t *testing.T) {
		goalsTestHome(t)
		ctx := context.Background()
		if migrateHeartbeatToSession(ctx, "hb-none-a", "hb-none-b") {
			t.Fatal("migrated without a source")
		}
		if migrateHeartbeatToSession(ctx, "same", "same") {
			t.Fatal("migrated onto itself")
		}
	})
}
