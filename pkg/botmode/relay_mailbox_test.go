package botmode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Two valid Desktop roster rows as the relay receives them
func relayRows() []any {
	return []any{
		pyjson.ObjectOf("profile", "default", "handle", "icebath", "connection_id", "cloud-1",
			"connection_label", "Icebath Cloud", "title", "Moxie", "description", "Main cloud agent"),
		pyjson.ObjectOf("profile", "researcher", "handle", "researcher", "connection_id", "ssh-vps", "connection_label", "VPS"),
	}
}

// Decoded reply record at a relay reply path
func readReplyRecord(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]any{}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestRosterRoundtripAndValidation(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := t.TempDir()
	rows := append(relayRows(),
		pyjson.ObjectOf("profile", "", "handle", "x", "connection_id", "c"),
		pyjson.ObjectOf("profile", "bad name!", "connection_id", "c"),
		"not-a-dict",
		pyjson.ObjectOf("profile", "default", "handle", "icebath", "connection_id", "cloud-1"))
	count, err := WriteRemoteRoster(context.Background(), root, rows)
	if err != nil || count != 2 {
		t.Fatalf("count %d %v", count, err)
	}
	back := ReadRemoteRoster(root)
	if len(back) != 2 || back[0].Profile != "default" || back[1].Profile != "researcher" {
		t.Fatalf("roster %+v", back)
	}
	if back[0].Title != "Moxie" {
		t.Fatalf("title %q", back[0].Title)
	}
}

func TestWriteReplyValidatesEnvelopeID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx, root := context.Background(), t.TempDir()
	var invalid *InvalidEnvelopeError
	if _, err := WriteReply(ctx, root, "../../etc/passwd", "x", "", ""); !errors.As(err, &invalid) {
		t.Fatalf("traversal id accepted: %v", err)
	}
	path, err := WriteReply(ctx, root, strings.Repeat("a", 32), "pong", "", "")
	if err != nil {
		t.Fatal(err)
	}
	record := readReplyRecord(t, path)
	if record["reply"] != "pong" || record["error"] != "" {
		t.Fatalf("record %v", record)
	}
}

func TestWriteReplyKeepsTheFirstSettledReplyForAnEnvelope(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx, root := context.Background(), t.TempDir()
	envID := strings.Repeat("a", 32)
	first, err := WriteReply(ctx, root, envID, "the answer", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := WriteReply(ctx, root, envID, "", "target busy", "target_busy")
	if err != nil || second != first {
		t.Fatalf("second %q %v", second, err)
	}
	record := readReplyRecord(t, first)
	if record["reply"] != "the answer" || record["error"] != "" || record["reason"] != "" {
		t.Fatalf("record %v", record)
	}
}

func TestWriteReplyReasonPassthroughAndClassification(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx, root := context.Background(), t.TempDir()
	path, err := WriteReply(ctx, root, strings.Repeat("c", 32), "", "boom", "delivery_timeout")
	if err != nil {
		t.Fatal(err)
	}
	if record := readReplyRecord(t, path); record["reason"] != "delivery_timeout" || record["error"] != "boom" {
		t.Fatalf("explicit reason %v", record)
	}
	path, err = WriteReply(ctx, root, strings.Repeat("d", 32), "", "Error code: 429 - rate limit", "")
	if err != nil {
		t.Fatal(err)
	}
	if record := readReplyRecord(t, path); record["reason"] != "provider_rate_limit" {
		t.Fatalf("classified reason %v", record)
	}
	path, err = WriteReply(ctx, root, strings.Repeat("e", 32), "ok", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if record := readReplyRecord(t, path); record["reason"] != "" || record["reply"] != "ok" {
		t.Fatalf("success reply %v", record)
	}
	if _, err := os.Stat(filepath.Join(RelayRoot(root), RepliesDir)); err != nil {
		t.Fatal(err)
	}
}
