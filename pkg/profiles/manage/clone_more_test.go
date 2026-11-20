package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Source home whose skills foo links into an external_dirs root and whose skills local is physical
func homeWithLinkedSkill(t *testing.T, tmp string) (string, string) {
	t.Helper()
	def := filepath.Join(tmp, ".icebath")
	external := filepath.Join(tmp, "agents-skills")
	writeFile(t, filepath.Join(external, "foo", "SKILL.md"), "# external foo\n")
	writeFile(t, filepath.Join(def, "skills", "local", "SKILL.md"), "# local\n")
	writeFile(t, filepath.Join(def, "config.yaml"), "model: test\nskills:\n  external_dirs:\n    - "+external+"\n")
	return def, external
}

// Distinct resolved SKILL.md candidates for a skill name across dirs through the direct and index lookups
func skillCandidateCount(name string, dirs []string) int {
	seen := map[string]bool{}
	record := func(md string) {
		if r, err := filepath.EvalSymlinks(md); err == nil {
			seen[r] = true
		}
	}
	for _, d := range dirs {
		if st, err := os.Stat(filepath.Join(d, name)); err == nil && st.IsDir() {
			if _, err := os.Stat(filepath.Join(d, name, "SKILL.md")); err == nil {
				record(filepath.Join(d, name, "SKILL.md"))
			}
		}
		for _, found := range skills.IterSkillIndexFiles(d, "SKILL.md") {
			if filepath.Base(filepath.Dir(found)) == name {
				record(found)
			}
		}
	}
	return len(seen)
}

func clonedModel(t *testing.T, dir string) map[string]any {
	t.Helper()
	cfg, err := config.ReadUserConfigRaw(context.Background(), filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCloneRecreatesSkillJunctionsAndSkipsDanglingOnes(t *testing.T) {
	for _, opts := range []CreateOptions{{CloneConfig: true}, {CloneAll: true}} {
		t.Run(fmt.Sprintf("clone_all=%v", opts.CloneAll), func(t *testing.T) {
			tmp := manageEnv(t)
			captureOut(t)
			def, external := homeWithLinkedSkill(t, tmp)
			writeFile(t, filepath.Join(def, "skills", "foo", "SKILL.md"), "# a physical copy would come from here\n")
			if err := os.MkdirAll(filepath.Join(def, "skills", "gone"), 0o755); err != nil {
				t.Fatal(err)
			}
			targets := map[string]string{
				filepath.Join(def, "skills", "foo"):  filepath.Join(external, "foo"),
				filepath.Join(def, "skills", "gone"): filepath.Join(tmp, "nowhere"),
			}
			oldTarget, oldCreate := junctionTarget, createJunction
			t.Cleanup(func() { junctionTarget, createJunction = oldTarget, oldCreate })
			junctionTarget = func(p string) (string, bool) { v, ok := targets[p]; return v, ok }
			createJunction = func(target, link string) error {
				if !isDir(target) {
					return errors.New("target missing")
				}
				return os.Symlink(target, link)
			}

			clone, err := CreateProfile(context.Background(), "clone", opts)
			if err != nil {
				t.Fatal(err)
			}
			foo := filepath.Join(clone, "skills", "foo")
			st, err := os.Lstat(foo)
			if err != nil || st.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("foo is not a link: %v %v", st, err)
			}
			got, _ := filepath.EvalSymlinks(foo)
			want, _ := filepath.EvalSymlinks(filepath.Join(external, "foo"))
			if got != want {
				t.Fatalf("foo resolves to %s, want %s", got, want)
			}
			if readText(t, filepath.Join(foo, "SKILL.md")) != "# external foo\n" {
				t.Fatal("foo does not read through to the external skill")
			}
			if st, err := os.Stat(filepath.Join(clone, "skills", "local", "SKILL.md")); err != nil || !st.Mode().IsRegular() {
				t.Fatal("physical skill was not copied")
			}
			if _, err := os.Lstat(filepath.Join(clone, "skills", "gone")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("dangling junction was copied")
			}
			if n := skillCandidateCount("foo", []string{filepath.Join(clone, "skills"), external}); n != 1 {
				t.Fatalf("foo has %d candidates", n)
			}
		})
	}
}

func TestSyncImportsRequiresACloneSource(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	_, err := CreateProfile(context.Background(), "lonely", CreateOptions{SyncImports: true})
	var ve *profiles.ValueError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "--sync-imports requires") {
		t.Fatalf("got %v", err)
	}
}

func TestCloneAllDoesNotCopyCronJobs(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	def := filepath.Join(tmp, ".icebath")
	writeFile(t, filepath.Join(def, "config.yaml"), "model: test")
	writeFile(t, filepath.Join(def, "cron", "jobs.json"), `{"jobs": [{"id": "abc123def456"}]}`)
	if err := os.MkdirAll(filepath.Join(def, "cron", "output"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := CreateProfile(context.Background(), "coder", CreateOptions{CloneAll: true})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "cron"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("cron dir holds %v (%v)", entries, err)
	}
	if clonedModel(t, dir)["model"] != "test" {
		t.Fatal("model was not cloned")
	}
}

func TestCloneAllDoesNotInheritTheSourceScreenOrBrowserProcessArtifacts(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	def := filepath.Join(tmp, ".icebath")
	writeFile(t, filepath.Join(def, "config.yaml"), "model: test")
	bd := filepath.Join(def, "bot-desktop")
	browser := filepath.Join(bd, "browser-profile")
	writeFile(t, filepath.Join(browser, "Default", "Cookies"), "jar")
	writeFile(t, filepath.Join(bd, "launcher.pid"), "4242 1.5")
	writeFile(t, filepath.Join(bd, "env"), "DISPLAY=:21\nXAUTHORITY=/x\n")
	lease, _ := json.Marshal(map[string]any{"holder": "human", "viewer_id": "v", "epoch": 3})
	writeFile(t, filepath.Join(bd, "lease.json"), string(lease))
	markers := []string{"DevToolsActivePort", "SingletonLock", "SingletonCookie", "SingletonSocket"}
	for _, m := range markers {
		writeFile(t, filepath.Join(browser, m), "source-process")
	}
	dir, err := CreateProfile(context.Background(), "coder", CreateOptions{CloneAll: true})
	if err != nil {
		t.Fatal(err)
	}
	cloned := filepath.Join(dir, "bot-desktop", "browser-profile")
	for _, f := range []string{"launcher.pid", "env", "lease.json"} {
		if pathExists(filepath.Join(dir, "bot-desktop", f)) {
			t.Fatal(f)
		}
	}
	for _, m := range markers {
		if pathExists(filepath.Join(cloned, m)) {
			t.Fatal(m)
		}
	}
	if readText(t, filepath.Join(cloned, "Default", "Cookies")) != "jar" {
		t.Fatal("browser data was not cloned")
	}
}

func TestCloneFromNamedProfile(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	ctx := context.Background()
	source, err := CreateProfile(ctx, "source", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "config.yaml"), "model: cloned")
	writeFile(t, filepath.Join(source, ".env"), "SECRET=yes")
	from := "source"
	target, err := CreateProfile(ctx, "target", CreateOptions{CloneFrom: &from, CloneConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg := clonedModel(t, target)
	if fmt.Sprint(cfg["_config_version"]) != fmt.Sprint(config.Defaults()["_config_version"]) || cfg["model"] != "cloned" {
		t.Fatalf("cloned config %v", cfg)
	}
	if strings.TrimSpace(readText(t, filepath.Join(target, ".env"))) != "SECRET=yes" {
		t.Fatal(".env was not cloned")
	}
}
