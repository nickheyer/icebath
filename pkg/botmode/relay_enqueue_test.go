package botmode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Relay home with the two Desktop rows synced into its roster
func syncedRelayRoot(t *testing.T) (context.Context, string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := t.TempDir()
	ctx := context.Background()
	if _, err := WriteRemoteRoster(ctx, root, relayRows()); err != nil {
		t.Fatal(err)
	}
	return ctx, root
}

// Scout on cloud-1 as a resolved target row
func scoutTarget() RemoteRosterRow {
	return RemoteRosterRow{Profile: "scout", Handle: "scout", ConnectionID: "cloud-1"}
}

// Whether the outbox holds the envelope's file
func outboxHolds(root string, envelope *pyjson.Object) bool {
	id, _ := envelope.GetOr("id", "").(string)
	_, err := os.Stat(filepath.Join(RelayRoot(root), OutboxDir, id+".json"))
	return err == nil
}

func TestRosterReadMissingAndCorrupt(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := t.TempDir()
	if rows := ReadRemoteRoster(root); len(rows) != 0 {
		t.Fatalf("missing roster read %v", rows)
	}
	base := RelayRoot(root)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, RosterFile), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rows := ReadRemoteRoster(root); len(rows) != 0 {
		t.Fatalf("corrupt roster read %v", rows)
	}
}

func TestResolveRemoteTargetForms(t *testing.T) {
	_, root := syncedRelayRoot(t)
	roster := ReadRemoteRoster(root)
	for raw, want := range map[string][2]string{
		"researcher": {"ssh-vps", "researcher"}, "@icebath": {"cloud-1", "default"}, "default": {"cloud-1", "default"},
		"icebath@cloud-1": {"cloud-1", "default"}, "default@cloud-1": {"cloud-1", "default"},
	} {
		row, ambiguous := ResolveRemoteTarget(raw, roster)
		if row == nil || ambiguous || row.ConnectionID != want[0] || row.Profile != want[1] {
			t.Fatalf("%s resolved %+v ambiguous %v", raw, row, ambiguous)
		}
	}
	for _, raw := range []string{"icebath@nope", "ghost"} {
		if row, ambiguous := ResolveRemoteTarget(raw, roster); row != nil || ambiguous {
			t.Fatalf("%s resolved %+v ambiguous %v", raw, row, ambiguous)
		}
	}
}

func TestResolveAmbiguousHandleAcrossConnections(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := t.TempDir()
	rows := append(relayRows(), pyjson.ObjectOf("profile", "researcher", "handle", "researcher", "connection_id", "cloud-1"))
	if _, err := WriteRemoteRoster(context.Background(), root, rows); err != nil {
		t.Fatal(err)
	}
	roster := ReadRemoteRoster(root)
	if row, ambiguous := ResolveRemoteTarget("researcher", roster); row != nil || !ambiguous {
		t.Fatalf("bare researcher resolved %+v ambiguous %v", row, ambiguous)
	}
	if row, _ := ResolveRemoteTarget("researcher@ssh-vps", roster); row == nil || row.ConnectionID != "ssh-vps" {
		t.Fatalf("qualified researcher resolved %+v", row)
	}
	forms := RemoteTargetForms(roster, nil)
	for _, form := range []string{"researcher@ssh-vps", "researcher@cloud-1", "icebath"} {
		if !slices.Contains(forms, form) {
			t.Fatalf("%s missing from %v", form, forms)
		}
	}
}

func TestEnqueueClaimReplaysExactIdentityUntilReply(t *testing.T) {
	ctx, root := syncedRelayRoot(t)
	target, _ := ResolveRemoteTarget("researcher", ReadRemoteRoster(root))
	envelope, err := EnqueueEnvelope(ctx, root, *target, "hi", "work", "work")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := envelope.GetOr("id", "").(string)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("id %q", id)
	}
	claimed, err := ClaimPendingEnvelopes(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].GetOr("id", nil) != id {
		t.Fatalf("claimed %v", claimed)
	}
	if claimed[0].GetOr("target_connection", nil) != "ssh-vps" || claimed[0].GetOr("message", nil) != "hi" {
		t.Fatalf("claimed %v", claimed[0])
	}
	again, err := ClaimPendingEnvelopes(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(claimed) || !pyjson.Equal(again[0], claimed[0]) {
		t.Fatalf("replay %v differs from %v", again, claimed)
	}
	if _, err := WriteReply(ctx, root, id, "done", nil, nil); err != nil {
		t.Fatal(err)
	}
	if rest, err := ClaimPendingEnvelopes(ctx, root); err != nil || len(rest) != 0 {
		t.Fatalf("after reply %v %v", rest, err)
	}
}

func TestEnqueueFailsFastWhenRowExplicitlyOffline(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := t.TempDir()
	ctx := context.Background()
	if _, err := WriteRemoteRoster(ctx, root, []any{pyjson.ObjectOf("profile", "scout", "handle", "scout",
		"connection_id", "cloud-1", "online", false)}); err != nil {
		t.Fatal(err)
	}
	roster := ReadRemoteRoster(root)
	if roster[0].Online == nil || *roster[0].Online {
		t.Fatalf("online %v", roster[0].Online)
	}
	_, err := EnqueueEnvelope(ctx, root, roster[0], "hi", "default", "icebath")
	var refused *EnvelopeRefusedError
	if !errors.As(err, &refused) || refused.Reason != "runtime_offline" || !strings.Contains(refused.Error(), "offline") {
		t.Fatalf("enqueue answered %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(RelayRoot(root), OutboxDir, "*.json"))
	if len(matches) != 0 {
		t.Fatalf("outbox holds %v", matches)
	}
}

func TestEnqueueFailsFastWhenTargetAbsentFromFreshRoster(t *testing.T) {
	ctx, root := syncedRelayRoot(t)
	_, err := EnqueueEnvelope(ctx, root, scoutTarget(), "hi", "default", "icebath")
	var refused *EnvelopeRefusedError
	if !errors.As(err, &refused) || refused.Reason != "runtime_offline" {
		t.Fatalf("enqueue answered %v", err)
	}
}

func TestEnqueueFailsOpenWhenLivenessUnknown(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := t.TempDir()
	ctx := context.Background()
	first, err := EnqueueEnvelope(ctx, root, scoutTarget(), "hi", "default", "icebath")
	if err != nil || !outboxHolds(root, first) {
		t.Fatalf("no roster: %v %v", first, err)
	}
	if _, err := WriteRemoteRoster(ctx, root, relayRows()); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-(RosterFreshSeconds + 5) * time.Second)
	if err := os.Chtimes(filepath.Join(RelayRoot(root), RosterFile), old, old); err != nil {
		t.Fatal(err)
	}
	second, err := EnqueueEnvelope(ctx, root, scoutTarget(), "hi again", "default", "icebath")
	if err != nil || !outboxHolds(root, second) {
		t.Fatalf("stale roster: %v %v", second, err)
	}
	if _, err := WriteRemoteRoster(ctx, root, relayRows()); err != nil {
		t.Fatal(err)
	}
	third, err := EnqueueEnvelope(ctx, root, ReadRemoteRoster(root)[1], "hello", "default", "icebath")
	if err != nil || !outboxHolds(root, third) {
		t.Fatalf("fresh roster without online flag: %v %v", third, err)
	}
}
