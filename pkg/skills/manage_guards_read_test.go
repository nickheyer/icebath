package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

type emptySecretScope struct{}

func (emptySecretScope) ManagedDotenvKeys() []string                      { return nil }
func (emptySecretScope) SourceSuppliedNames() []string                    { return nil }
func (emptySecretScope) BuildProfileSecretScope(string) map[string]string { return map[string]string{} }
func (emptySecretScope) CurrentSecretScope(context.Context) map[string]string {
	return map[string]string{}
}

// Runs the registered read_file tool over env and returns its JSON output
func readFileThroughRegistry(t *testing.T, env *tools.Env, args map[string]any, taskID string) string {
	t.Helper()
	if tools.ProfileSecrets == nil {
		tools.ProfileSecrets = emptySecretScope{}
		t.Cleanup(func() { tools.ProfileSecrets = nil })
	}
	d := tools.Default.Get("read_file")
	if d == nil {
		t.Fatal("read_file is not registered")
	}
	return d.Handler(context.Background(), &tools.Call{Name: "read_file", Args: args, TaskID: taskID, Env: env})
}

func TestReadFileRegistersBackgroundReviewReadMark(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(target, []byte("---\nname: t\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{Home: filepath.Join(dir, "home"), Config: config.Defaults(), Values: &sync.Map{}, Cwd: dir}
	SetCurrentWriteOrigin(env, BackgroundReview)
	ResetBackgroundReviewReadMarks(env)
	if backgroundReviewHasRead(env, target) {
		t.Fatal("fresh read set already marked")
	}
	if out := readFileThroughRegistry(t, env, map[string]any{"path": target}, "bg-review-test"); !strings.Contains(out, "body") {
		t.Fatalf("read_file %s", out)
	}
	if !backgroundReviewHasRead(env, target) {
		t.Fatal("full read_file inside a review fork must register with the read-before-write guard")
	}
	ResetBackgroundReviewReadMarks(env)
	readFileThroughRegistry(t, env, map[string]any{"path": target, "offset": 2}, "bg-review-test2")
	if backgroundReviewHasRead(env, target) {
		t.Fatal("a partial read satisfied the guard")
	}
}

func TestReadFileOutsideReviewDoesNotMark(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(target, []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{Home: filepath.Join(dir, "home"), Config: config.Defaults(), Values: &sync.Map{}, Cwd: dir}
	ResetBackgroundReviewReadMarks(env)
	readFileThroughRegistry(t, env, map[string]any{"path": target}, "fg-test")
	if backgroundReviewHasRead(env, target) {
		t.Fatal("foreground read populated the review read set")
	}
}
