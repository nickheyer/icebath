package manage

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

// Deletes the imported profile and checks both its directory and wrapper are gone
func deleteAndCheckWrapper(t *testing.T, profileDir, wrapper string) {
	t.Helper()
	captureOut(t)
	if _, err := DeleteProfile(context.Background(), "imported", true); err != nil {
		t.Fatal(err)
	}
	if exists(profileDir) || exists(wrapper) {
		t.Fatalf("profile dir or wrapper survived: %v %v", exists(profileDir), exists(wrapper))
	}
}

func TestDeleteRemovesTheProfileWrapper(t *testing.T) {
	manageEnv(t)
	dir, err := CreateProfile(context.Background(), "imported", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := profiles.CreateWrapperScript("imported", "")
	if err != nil || wrapper == "" {
		t.Fatalf("%q %v", wrapper, err)
	}
	deleteAndCheckWrapper(t, dir, wrapper)
}
