package manage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestDeleteRemovesAPreBatWindowsWrapper(t *testing.T) {
	tmp := manageEnv(t)
	dir, err := CreateProfile(context.Background(), "imported", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(tmp, ".local", "bin", "imported")
	writeFile(t, wrapper, "@echo off\r\nicebath -p imported %*\r\n")
	deleteAndCheckWrapper(t, dir, wrapper)
}
