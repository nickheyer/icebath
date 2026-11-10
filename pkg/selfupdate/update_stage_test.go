package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Status file a shim of pid 80335 writes, with no exported path in the environment
func usStatusFile(t *testing.T, tmp string) string {
	t.Helper()
	t.Setenv(UpdateStatusFileEnv, "")
	os.Unsetenv(UpdateStatusFileEnv)
	return filepath.Join(tmp, "icebath-update-status.80335")
}

func usAssertRunning(t *testing.T, path, message string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := pyjson.Loads(string(data))
	obj, ok := doc.(*pyjson.Object)
	if err != nil || !ok {
		t.Fatalf("payload %q", data)
	}
	if status, _ := obj.Get("status"); status != "running" {
		t.Fatalf("only the shim publishes terminal states, got %v", status)
	}
	if got, _ := obj.Get("message"); got != message {
		t.Fatalf("message %v", got)
	}
}

func TestPublishesThroughExportedEnvVar(t *testing.T) {
	status := usStatusFile(t, t.TempDir())
	t.Setenv(UpdateStatusFileEnv, status)
	PublishStage("Building the web UI")
	usAssertRunning(t, status, "Building the web UI")
}

func TestMarkerFallbackCoversTheOldShim(t *testing.T) {
	tmp := t.TempDir()
	status := usStatusFile(t, tmp)
	t.Setenv("ICEBATH_HOME", tmp)
	vsWrite(t, filepath.Join(tmp, updaterecord.MarkerName), fmt.Sprintf("80335\n%d\n", time.Now().Unix()))
	vsWrite(t, status, `{"status":"running","message":"old"}`)
	t.Setenv("TMPDIR", tmp)
	PublishStage("Updating Python dependencies (PM)")
	usAssertRunning(t, status, "Updating Python dependencies (PM)")
}

func TestMarkerFallbackUsesThePlatformHomeWithoutEnvVar(t *testing.T) {
	tmp := t.TempDir()
	status := usStatusFile(t, tmp)
	t.Setenv("ICEBATH_HOME", "")
	os.Unsetenv("ICEBATH_HOME")
	t.Setenv("HOME", filepath.Join(tmp, "platform"))
	platform := home.PlatformDefault()
	vsWrite(t, filepath.Join(platform, updaterecord.MarkerName), fmt.Sprintf("80335\n%d\n", time.Now().Unix()))
	vsWrite(t, status, `{"status":"running","message":"old"}`)
	t.Setenv("TMPDIR", tmp)
	PublishStage("Building products")
	usAssertRunning(t, status, "Building products")
}

func TestNoUISourcesIsInert(t *testing.T) {
	tmp := t.TempDir()
	status := usStatusFile(t, tmp)
	t.Setenv("ICEBATH_HOME", tmp)
	t.Setenv("TMPDIR", tmp)
	PublishStage("anything")
	if _, err := os.Stat(status); err == nil {
		t.Fatal("a stage was published with no UI watching")
	}
}

func TestPublishNeverRaisesOnUnwritableTarget(t *testing.T) {
	t.Setenv(UpdateStatusFileEnv, filepath.Join(t.TempDir(), "no", "such", "dir", "f"))
	PublishStage("boom")
}
