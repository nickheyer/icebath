//go:build windows

package pm

import (
	"context"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStageOnlyStageRepinRefusesANativeDirectoryHoldThenRecovers(t *testing.T) {
	store, _ := ieSandbox(t)
	ctx := context.Background()
	ieSwapRegistry(t, &ieStagePackage{BasePackage{Name: "stage-test"}})
	original, replacement := []byte("original"), []byte("replacement")
	ieArmLock(t, store, []any{map[string]any{"url": "https://example.test/tool", "sha256": hexSHA256(original)}})
	ieSeedFetch(t, store, "payload.bin", original)
	entry, err := stageOnly(ctx, "stage-test", "linux-arm64-bionic", StageOnlyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	marker := readFile(t, filepath.Join(entry, ".pm-stage-pin.json"))
	ieArmLock(t, store, []any{map[string]any{"url": "https://example.test/tool", "sha256": hexSHA256(replacement)}})
	ieSeedFetch(t, store, "payload.bin", replacement)
	name, _ := windows.UTF16PtrFromString(entry)
	handle, err := windows.CreateFile(name, 1, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = stageOnly(ctx, "stage-test", "linux-arm64-bionic", StageOnlyOptions{})
	held := string(readFile(t, filepath.Join(entry, "bin/tool")))
	heldMarker := string(readFile(t, filepath.Join(entry, ".pm-stage-pin.json")))
	windows.CloseHandle(handle)
	if !isInstallError(err) || held != "original" || heldMarker != string(marker) {
		t.Fatalf("held repin %v left %q", err, held)
	}
	if _, err := stageOnly(ctx, "stage-test", "linux-arm64-bionic", StageOnlyOptions{}); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, filepath.Join(entry, "bin/tool"))) != "replacement" {
		t.Fatal("repin did not recover")
	}
}
