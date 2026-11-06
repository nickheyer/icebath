package skills

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

func TestDoListPassesNoPlatformToDisabledSkillNames(t *testing.T) {
	x, _, _ := newTestCLI(t)
	t.Setenv("ICEBATH_PLATFORM", "telegram")
	seen := []string{}
	x.seams.disabledSkills = func(platform string) map[string]bool {
		seen = append(seen, platform)
		return map[string]bool{}
	}
	if err := x.DoList(context.Background(), "all", false, nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "" {
		t.Fatalf("disabled skill names asked for platforms %q", seen)
	}
}

func bundledSkillDir(t *testing.T, name string) string {
	t.Helper()
	for _, b := range discoverBundledSkills(bundledTree()) {
		if b.name == name {
			return b.dir
		}
	}
	t.Fatalf("no bundled skill %s", name)
	return ""
}

func bundledInstallCLI(t *testing.T) (*HubCLI, string) {
	t.Helper()
	t.Setenv("ICEBATH_BUNDLED_SKILLS", "")
	t.Setenv("ICEBATH_OPTIONAL_SKILLS", "")
	x, _, _ := newTestCLI(t)
	x.Sources = func() []SkillSource { t.Fatal("a bundled skill must not hit the hub"); return nil }
	return x, filepath.Dir(x.S.SkillsDir())
}

func TestInstallByNameRestoresTheBundledSkillItNames(t *testing.T) {
	for _, identifier := range []string{"computer-use", "NousResearch/hermes-agent/skills/productivity/pdf",
		"skills-sh/nousresearch/hermes-agent/google-workspace"} {
		t.Run(identifier, func(t *testing.T) {
			x, tmp := bundledInstallCLI(t)
			ctx := context.Background()
			writeTestFile(t, filepath.Join(x.S.Home, "config.yaml"), "curator:\n  prune_builtins: true\n")
			name := path.Base(identifier)
			if err := x.S.toggleSuppressedName(name, true); err != nil {
				t.Fatal(err)
			}
			usage, entry := pyjson.NewObject(), pyjson.NewObject()
			entry.Set("state", StateArchived)
			usage.Set(name, entry)
			if !x.S.SaveUsage(usage) {
				t.Fatal("usage not saved")
			}
			category := filepath.Join(x.S.SkillsDir(), path.Base(path.Dir(bundledSkillDir(t, name))))
			outside := filepath.Join(tmp, "outside")
			os.MkdirAll(outside, 0o755)
			os.MkdirAll(filepath.Dir(category), 0o755)
			if err := os.Symlink(outside, category); err != nil {
				t.Fatal(err)
			}
			sink := &bytes.Buffer{}
			o := HubInstallOptions{SkipConfirm: true, Console: &pyrich.Console{Out: sink, Width: 80}}
			if got, err := x.DoInstall(ctx, identifier, o); err != nil || got != HubFailed {
				t.Fatalf("restore through a symlinked category = %v %v", got, err)
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Fatalf("restore escaped the skills tree: %v", entries)
			}
			os.Remove(category)
			if got, err := x.DoInstall(ctx, identifier, o); err != nil || got != HubOK {
				t.Fatalf("restore = %v %v\n%s", got, err, sink.String())
			}
			if !strings.Contains(sink.String(), "Restored built-in skill") {
				t.Fatal(sink.String())
			}
			manifest, err := x.S.readManifest()
			if _, ok := manifest[name]; err != nil || !ok {
				t.Fatalf("manifest %v %v", manifest, err)
			}
			active := false
			for _, md := range iterSkillMDs(x.S.SkillsDir(), false) {
				active = active || readSkillName(md, "") == name
			}
			suppressed, _ := x.S.ReadSuppressedNames()
			if !active || suppressed[name] {
				t.Fatalf("active %v suppressed %v", active, suppressed)
			}
			loaded, _ := x.S.LoadUsage()
			rec, _ := loaded.Get(name)
			state, _ := rec.(*pyjson.Object).Get("state")
			if state != StateActive {
				t.Fatalf("usage state %v", state)
			}
			if got, err := x.DoInstall(ctx, identifier, o); err != nil || got != HubNoop {
				t.Fatalf("already active install = %v %v", got, err)
			}
			stranger := filepath.Join(x.S.SkillsDir(), "hub-installs", name)
			writeTestFile(t, filepath.Join(stranger, "SKILL.md"), "---\nname: "+name+"\n---\nstranger\n")
			if err := x.Hub.LockFile("").RecordInstall(name, "skills-sh", "skills-sh/x/y/"+name, "community", "safe", "h",
				"hub-installs/"+name, []string{"SKILL.md"}, nil, nil); err != nil {
				t.Fatal(err)
			}
			sink.Reset()
			if got, err := x.DoInstall(ctx, identifier, o); err != nil || got != HubFailed {
				t.Fatalf("shadowed install = %v %v", got, err)
			}
			errLine := ""
			for _, line := range strings.Split(sink.String(), "\n") {
				if strings.HasPrefix(line, "Error:") && errLine == "" {
					errLine = line
				}
			}
			if !strings.Contains(errLine, "'"+name+"' is a built-in skill") || !strings.Contains(errLine, "`icebath skills uninstall "+name+"`") ||
				strings.Contains(sink.String(), "already available") {
				t.Fatalf("shadow error %q in\n%s", errLine, sink.String())
			}
			if !strings.HasSuffix(mustRead(t, filepath.Join(stranger, "SKILL.md")), "stranger\n") {
				t.Fatal("stranger skill overwritten")
			}
		})
	}
}

func TestBuiltInRestoreSurvivesLeftoversAndRecordsEveryOutcomeAsBundled(t *testing.T) {
	x, tmp := bundledInstallCLI(t)
	ctx := context.Background()
	var rows [][2]string
	x.seams.recordInstall = func(_ context.Context, _, source string, _ any, outcome string, _, _ any, _ error) {
		rows = append(rows, [2]string{source, outcome})
	}
	dest := filepath.Join(x.S.SkillsDir(), path.Base(path.Dir(bundledSkillDir(t, "obsidian"))), "obsidian")
	o := HubInstallOptions{SkipConfirm: true, Console: &pyrich.Console{Out: &bytes.Buffer{}, Width: 80}}
	writeTestFile(t, filepath.Join(dest, "wiki", "index.md"), "mine\n")
	if got, err := x.DoInstall(ctx, "obsidian", o); err != nil || got != HubOK {
		t.Fatalf("restore over a leftover dir = %v %v", got, err)
	}
	if st, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil || !st.Mode().IsRegular() || mustRead(t, filepath.Join(dest, "wiki", "index.md")) != "mine\n" {
		t.Fatal("restore lost the skill or the user data", err)
	}
	real := filepath.Join(tmp, "real-category")
	if err := os.Rename(filepath.Dir(dest), real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Dir(dest)); err != nil {
		t.Fatal(err)
	}
	if got, err := x.DoInstall(ctx, "obsidian", o); err != nil || got != HubNoop {
		t.Fatalf("present behind a symlinked category = %v %v", got, err)
	}
	os.Remove(filepath.Join(real, "obsidian", "SKILL.md"))
	if got, err := x.DoInstall(ctx, "obsidian", o); err != nil || got != HubFailed {
		t.Fatalf("missing behind the symlink = %v %v", got, err)
	}
	if len(rows) != 2 || rows[0] != [2]string{"bundled", "success"} || rows[1] != [2]string{"bundled", "failed"} {
		t.Fatalf("recorded %v", rows)
	}
}
