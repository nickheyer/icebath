package manage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestStoredWebappSignInStateIsPrivateAndNeverExported(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	if !profiles.ProfilePathIsPrivate([]string{"webapp", "session-token"}) || !profiles.ProfilePathIsPrivate([]string{"WebApp"}) {
		t.Fatal("the webapp store is not a credential path")
	}
	dir, _ := CreateProfile(context.Background(), "coder", CreateOptions{})
	writeFile(t, filepath.Join(dir, "webapp", "session-token"), "token")
	writeFile(t, filepath.Join(dir, "notes.md"), "plain notes\n")
	archive, err := ExportProfile("coder", filepath.Join(tmp, "out.tar.gz"), nil)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := ImportProfile(archive, "copy")
	if err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(imported, "webapp")) || readText(t, filepath.Join(imported, "notes.md")) != "plain notes\n" {
		t.Fatal("the stored webapp sign in state was exported")
	}
}

func TestCloneAllLeavesTheSourceWebappSignInStateBehind(t *testing.T) {
	tmp := manageEnv(t)
	named := namedProfile(t, tmp, "src")
	writeFile(t, filepath.Join(named, "webapp", "session-token"), "token")
	writeFile(t, filepath.Join(named, "skills", "webapp", "SKILL.md"), "skill")
	if ignored := cloneAllCopytreeIgnore(named)(named, []string{"webapp", "skills"}); !ignored["webapp"] || ignored["skills"] {
		t.Fatalf("root %v", ignored)
	}
	if below := cloneAllCopytreeIgnore(named)(filepath.Join(named, "skills"), []string{"webapp"}); below["webapp"] {
		t.Fatalf("below root %v", below)
	}
}
