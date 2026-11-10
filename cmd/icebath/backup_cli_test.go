package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestBackupEdgeCasesIncompleteArchiveIsKeptButReportedAsFailure(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	unreadable := filepath.Join(h, "skills", "locked.md")
	if err := os.MkdirAll(filepath.Dir(unreadable), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model:\n  provider: openrouter\n"), 0o644)
	os.WriteFile(unreadable, []byte("secret\n"), 0o644)
	os.Chmod(unreadable, 0)
	if f, err := os.Open(unreadable); err == nil {
		f.Close()
		t.Skip("running as root: chmod 0 does not make the file unreadable")
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Cleanup(home.PatchUserHome(tmp))
	out2 := filepath.Join(tmp, "out2.zip")
	var err error
	out := captureStream(t, &os.Stdout, func() { err = newCLI().Execute([]string{"backup", "--output", out2}) })
	if exit := leftCoreExit(t, err); exit.Code != 1 || exit.Msg != "" {
		t.Fatalf("exit %d %q", exit.Code, exit.Msg)
	}
	if !strings.Contains(out, "\nBackup incomplete: "+out2+"\n") {
		t.Fatalf("stdout %q", out)
	}
	if _, err := os.Stat(out2); err != nil {
		t.Fatal("the incomplete archive was not kept")
	}
	os.Chmod(unreadable, 0o600)
	out = captureStream(t, &os.Stdout, func() {
		err = newCLI().Execute([]string{"backup", "--output", filepath.Join(tmp, "out4.zip")})
	})
	if err != nil || !strings.Contains(out, "\nBackup complete: ") {
		t.Fatalf("%v %q", err, out)
	}
}
