package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

func TestIncompleteHandoffRequiresExplicitUpdateRetry(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ucPatchRoot(t, root)
	completions := 0
	saved := updateRunCompletion
	updateRunCompletion = func(context.Context, io.Writer, map[string]any) (map[string]any, error) {
		completions++
		return map[string]any{"exit_code": 0}, nil
	}
	t.Cleanup(func() { updateRunCompletion = saved })
	var out, errOut bytes.Buffer
	err := completeSourceUpdate(pm.WithCLIStderr(context.Background(), &errOut), &out, nil)
	var exit *pm.SystemExit
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("err = %v", err)
	}
	if completions != 0 {
		t.Fatal("the completion ran without a request")
	}
	if !strings.Contains(errOut.String(), "run `icebath update` again") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if strings.Contains(out.String(), "Update complete") {
		t.Fatalf("stdout = %q", out.String())
	}
	for _, marker := range []string{".update-incomplete", ".lazy-refresh-incomplete"} {
		if _, err := os.Lstat(filepath.Join(root, marker)); err == nil {
			t.Fatalf("%s was written", marker)
		}
	}
}
