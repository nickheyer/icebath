//go:build windows

package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"golang.org/x/sys/windows"
)

func TestATailChildTheJobRefusesNeverRuns(t *testing.T) {
	tmp := t.TempDir()
	root, started := vsTailRoot(t, tmp)
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(updaterecord.SwapUpdateTreeJob(event))
	ctx := context.Background()
	lock := updaterecord.NewUpdateLock(filepath.Join(tmp, "marker"), root, false)
	if ok, err := lock.AcquireCheckout(root); err != nil || !ok {
		t.Fatalf("checkout lock %v %v", ok, err)
	}
	err = finishSourceUpdate(ctx, root, true, filepath.Join(root, ".update-pending"))
	lock.Release(ctx)
	if err == nil || !strings.Contains(err.Error(), "source update completion failed") {
		t.Fatalf("refused bind %v", err)
	}
	if _, err := os.Stat(started); err == nil {
		t.Fatal("the tail child ran outside the update's job")
	}
}
