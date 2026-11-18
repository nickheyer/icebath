package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallReceiptMarkerUsesTheSharedSchema(t *testing.T) {
	root := t.TempDir()
	commit, branch := "abcdef1234567890", "main"
	marker, err := writeMarker(root, &commit, &branch, time.Now().UTC().Truncate(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, BootstrapMarkerName))
	if string(raw) != string(marker) {
		t.Fatalf("returned marker differs from disk:\n%s\n%s", marker, raw)
	}
	var disk map[string]any
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	if disk["schemaVersion"] != float64(1) || disk["pinnedCommit"] != commit || disk["pinnedBranch"] != branch {
		t.Fatalf("marker %v", disk)
	}
	if _, ok := disk["completedAtUnix"]; ok {
		t.Fatal("receipt must use the shared completedAt field")
	}
	if got := ISO8601UTC(time.UnixMilli(1_709_251_199_123)); got != "2024-02-29T23:59:59.123Z" {
		t.Fatalf("iso8601 %q", got)
	}
	completed, _ := disk["completedAt"].(string)
	if len(completed) != 24 || !strings.HasSuffix(completed, "Z") {
		t.Fatalf("completedAt must be ISO-8601 UTC: %q", completed)
	}
}

func TestInstallReceiptMarkerIsPublishedAtomicallyOverAPriorOne(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, BootstrapMarkerName), []byte("{\"schemaVersion\":1}"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit, branch := "abcdef1234567890", "main"
	if _, err := writeMarker(root, &commit, &branch, time.Now()); err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	raw, err := os.ReadFile(filepath.Join(root, BootstrapMarkerName))
	if err != nil || json.Unmarshal(raw, &disk) != nil || disk["pinnedCommit"] != commit {
		t.Fatalf("final marker %q %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(root, BootstrapMarkerName+".tmp")); err == nil {
		t.Fatal("temp sibling must not remain after atomic publish")
	}
}

func TestMarkerWriteFailureLeavesNoFinalMarker(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit, branch := "abcdef1234567890", "main"
	_, err := writeMarker(notADir, &commit, &branch, time.Now())
	if err == nil || !strings.Contains(err.Error(), "bootstrap marker") {
		t.Fatalf("error should mention the marker path: %v", err)
	}
	for _, name := range []string{BootstrapMarkerName, BootstrapMarkerName + ".tmp"} {
		if _, err := os.Stat(filepath.Join(notADir, name)); err == nil {
			t.Fatalf("%s left behind", name)
		}
	}
}
