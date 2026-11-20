package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/skills"
)

var skillsSourceChoices = []string{"all", "official", "skills-sh", "well-known", "github", "clawhub", "lobehub", "browse-sh",
	"nvidia", "openai", "anthropic", "huggingface", "voltagent", "gstack", "minimax"}

// Checks the argparse choices of one flag
func assertChoices(t *testing.T, c *CLI, path, flag string, want ...string) {
	t.Helper()
	got := c.find(path).Flags().Lookup(flag).Annotations["choices"]
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s --%s choices %v", path, flag, got)
	}
}

func TestCLISkillsTreeMatchesBuildSkillsParser(t *testing.T) {
	c := NewCLI()
	yes := func(help string) parserFlag { return parserFlag{"yes", "y", "bool", "false", help} }
	flag := func(name, help string) parserFlag { return parserFlag{name, "", "bool", "false", help} }
	assertParser(t, c, "skills", "Search, install, configure, and manage skills",
		"Search, install, inspect, audit, configure, and manage skills from skills.sh, well-known agent skill endpoints, GitHub, ClawHub, and other registries.", "")
	assertParser(t, c, "skills trust", "Trust a project so its repo-local skills (./.icebath/skills, ./.agents/skills) load", "",
		"  path   Project root to trust (default: enclosing git checkout of cwd)")
	assertParser(t, c, "skills untrust", "Revoke project-skill trust for a repo", "",
		"  path   Project root to untrust (default: enclosing git checkout of cwd)")
	assertParser(t, c, "skills browse", "Browse all available skills (paginated)", "", "",
		parserFlag{"page", "", "int", "1", "Page number (default: 1)"},
		parserFlag{"size", "", "int", "20", "Results per page (default: 20)"},
		parserFlag{"source", "", "string", "all", "Filter by source or provider (e.g. nvidia, openai) (default: all)"})
	assertChoices(t, c, "skills browse", "source", skillsSourceChoices...)
	assertParser(t, c, "skills search", "Search skill registries", "", "  query   Search query",
		parserFlag{"source", "", "string", "all", "Filter by source or provider (e.g. nvidia, openai)"},
		parserFlag{"limit", "", "int", "25", "Max results"},
		flag("json", "Output JSON instead of a table (full identifiers, scripting-friendly)"))
	assertChoices(t, c, "skills search", "source", skillsSourceChoices...)
	assertParser(t, c, "skills install", "Install a skill", "",
		"  identifier   Skill identifier (e.g. openai/skills/skill-creator) or a direct HTTP(S) URL to a SKILL.md file",
		parserFlag{"category", "", "string", "", "Category folder to install into"},
		parserFlag{"name", "", "string", "", "Override the skill name (useful when installing from a URL whose SKILL.md has no `name:` frontmatter)"},
		flag("force", "Install despite blocked scan verdict"), yes("Skip confirmation prompt (needed in TUI mode)"))
	assertParser(t, c, "skills inspect", "Preview a skill without installing", "", "  identifier   Skill identifier")
	assertParser(t, c, "skills list", "List installed skills", "", "",
		parserFlag{"source", "", "string", "all", ""},
		flag("enabled-only", "Hide disabled skills. Use with -p <profile> to see exactly which skills will load for that profile."))
	assertChoices(t, c, "skills list", "source", "all", "hub", "builtin", "local")
	assertParser(t, c, "skills check", "Check installed hub skills for updates", "", "  name   Specific skill to check (default: all)")
	assertParser(t, c, "skills update", "Update installed hub skills", "", "  name   Specific skill to update (default: all outdated skills)",
		flag("force", "Overwrite skills you have edited locally (they are skipped by default)"))
	assertParser(t, c, "skills audit", "Re-scan installed hub skills", "", "  name   Specific skill to audit (default: all)",
		flag("deep", "Run AST-level analysis on Python files (opt-in diagnostic)"))
	assertParser(t, c, "skills uninstall", "Remove a hub-installed skill", "", "  name   Skill name to remove", yes("Skip confirmation prompt"))
	assertParser(t, c, "skills reset", "Reset a bundled skill — clears 'user-modified' tracking so updates work again",
		"Clear a bundled skill's entry from the sync manifest (~/.icebath/skills/.bundled_manifest) so future 'icebath update' runs stop marking it as user-modified. Pass --restore to also replace the current copy with the bundled version.",
		"  name   Skill name to reset (e.g. google-workspace)",
		flag("restore", "Also delete the current copy and re-copy the bundled version"), yes("Skip confirmation prompt when using --restore"))
	assertParser(t, c, "skills list-modified", "List bundled skills you've edited (which `icebath update` keeps)",
		"Show the bundled skills whose local copy differs from the version last synced, i.e. the ones `icebath update` reports as user-modified and skips. Use `icebath skills diff <name>` to see changes and `icebath skills reset <name>` to resume updates.",
		"", flag("json", "Output the list as JSON"))
	assertParser(t, c, "skills diff", "Show how your copy of a bundled skill differs from the stock version",
		"Print a unified diff between your local copy of a bundled skill and the current bundled (stock) version, so you can confirm what changed before running `icebath skills reset`.",
		"  name   Skill name to diff (e.g. google-workspace)")
	assertParser(t, c, "skills opt-out", "Stop bundled skills from being seeded into this profile",
		"Write the .no-bundled-skills marker so the installer, `icebath update`, and any direct sync stop seeding bundled skills into the active profile. By default nothing already on disk is touched. Pass --remove to ALSO delete bundled skills that are unmodified (user-edited and hub/local skills are never removed).",
		"", flag("remove", "Also delete already-present unmodified bundled skills"), yes("Skip confirmation prompt when using --remove"))
	assertParser(t, c, "skills opt-in", "Re-enable bundled-skill seeding (undo opt-out)",
		"Remove the .no-bundled-skills marker so bundled skills are seeded again on the next `icebath update`. Pass --sync to re-seed now.",
		"", flag("sync", "Re-seed bundled skills immediately instead of waiting for update"))
	assertParser(t, c, "skills repair-official", "Backfill or restore official optional skills from repo source",
		"Repair official optional skill provenance. By default, only backfills hub metadata for exact matches. Pass --restore to replace missing or mutated active copies from optional-skills/, moving existing copies to a restore backup first. Use name 'all' to repair every optional skill.",
		"  name   Official optional skill folder/frontmatter name, or 'all'",
		flag("restore", "Restore from official optional source, backing up existing matching copies"), yes("Skip confirmation prompt when using --restore"))
	assertParser(t, c, "skills publish", "Publish a skill to a registry", "", "  skill_path   Path to skill directory",
		parserFlag{"to", "", "string", "github", "Target registry"}, parserFlag{"repo", "", "string", "", "Target GitHub repo (e.g. openai/skills)"})
	assertChoices(t, c, "skills publish", "to", "github", "clawhub")
	assertParser(t, c, "skills snapshot", "Export/import skill configurations", "", "")
	assertParser(t, c, "skills snapshot export", "Export installed skills to a file", "", "  output   Output JSON file path (use - for stdout)")
	assertParser(t, c, "skills snapshot import", "Import and install skills from a file", "", "  input   Input JSON file path",
		flag("force", "Force install despite caution verdict"))
	assertParser(t, c, "skills tap", "Manage skill sources", "", "")
	assertParser(t, c, "skills tap list", "List configured taps", "", "")
	assertParser(t, c, "skills tap add", "Add a GitHub repo as skill source", "", "  repo   GitHub repo (e.g. owner/repo)")
	assertParser(t, c, "skills tap remove", "Remove a tap", "", "  name   Tap name to remove")
	assertParser(t, c, "skills config", "Interactive skill configuration — enable/disable individual skills", "", "")
	var subs []string
	for _, sub := range c.find("skills").Commands() {
		subs = append(subs, sub.Name())
	}
	if strings.Join(subs, ",") != "trust,untrust,browse,search,install,inspect,list,check,update,audit,uninstall,reset,list-modified,diff,opt-out,opt-in,repair-official,publish,snapshot,tap,config" {
		t.Fatal(subs)
	}
}

func TestCLISkillsGroupsWithoutActionPrintUsage(t *testing.T) {
	dir := t.TempDir()
	if code, out, _ := runSkillsMain(t, dir, []string{"skills", "snapshot"}, nil); code != 0 || out != "Usage: icebath skills snapshot \n\n" {
		t.Fatalf("%d %q", code, out)
	}
	if code, out, _ := runSkillsMain(t, dir, []string{"skills", "tap"}, nil); code != 0 || out != "Usage: icebath skills tap \n\n" {
		t.Fatalf("%d %q", code, out)
	}
}

func TestCLISkillsConfigRequiresATerminal(t *testing.T) {
	old := skillsConfigStdinTTY
	t.Cleanup(func() { skillsConfigStdinTTY = old })
	skillsConfigStdinTTY = func() bool { return false }
	code, out, _ := runSkillsMain(t, t.TempDir(), []string{"skills", "config"}, nil)
	if code != 1 || out != "Error: 'icebath skills config' requires an interactive terminal.\nIt cannot be run through a pipe or non-interactive subprocess.\nRun it directly in your terminal instead.\n" {
		t.Fatalf("%d %q", code, out)
	}
}

func TestCLISkillsTrustAndUntrustAProject(t *testing.T) {
	dir := t.TempDir()
	project := t.TempDir()
	os.Mkdir(filepath.Join(project, ".git"), 0o755)
	root, _ := filepath.EvalSymlinks(project)
	trusted := func() bool {
		t.Helper()
		cfg, err := config.Load(dir, config.NewSecrets(dir).Lookup())
		if err != nil {
			t.Fatal(err)
		}
		return skills.NewScope(dir, cfg).IsProjectRootTrusted(root)
	}
	if code, out, _ := runSkillsMain(t, dir, []string{"skills", "trust", project}, nil); code != 0 ||
		out != "Trusted: "+root+"\nNo project skills found yet — add them under "+filepath.Join(".icebath", "skills")+" or "+filepath.Join(".agents", "skills")+".\n" || !trusted() {
		t.Fatalf("%d %q", code, out)
	}
	for _, d := range []string{filepath.Join(project, ".icebath", "skills", "one"), filepath.Join(project, ".agents", "skills", "two")} {
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: x\ndescription: y\n---\n"), 0o644)
	}
	want := "Already trusted: " + root + "\n2 project skill(s) will load in sessions started inside this repo (they take precedence over same-named profile skills).\n"
	if code, out, _ := runSkillsMain(t, dir, []string{"skills", "trust", project}, nil); code != 0 || out != want {
		t.Fatalf("%d %q", code, out)
	}
	if code, out, _ := runSkillsMain(t, dir, []string{"skills", "untrust", project}, nil); code != 0 || out != "Untrusted: "+root+"\nProject skills from this repo will no longer load.\n" || trusted() {
		t.Fatalf("%d %q", code, out)
	}
	if code, out, _ := runSkillsMain(t, dir, []string{"skills", "untrust", project}, nil); code != 0 || out != root+" was not trusted.\n" {
		t.Fatalf("%d %q", code, out)
	}
	missing := filepath.Join(root, "missing")
	if code, out, _ := runSkillsMain(t, dir, []string{"skills", "trust", missing}, nil); code != 0 || out != "Not a directory: "+missing+"\n" {
		t.Fatalf("%d %q", code, out)
	}
	plain := t.TempDir()
	t.Chdir(plain)
	t.Setenv("TERMINAL_CWD", plain)
	if code, out, _ := runSkillsMain(t, dir, []string{"skills", "trust"}, nil); code != 0 || out != "Not inside a git checkout. Run from a project directory or pass the project root path explicitly.\n" {
		t.Fatalf("%d %q", code, out)
	}
	t.Chdir(filepath.Join(project, ".agents"))
	t.Setenv("TERMINAL_CWD", filepath.Join(project, ".agents"))
	if code, out, _ := runSkillsMain(t, dir, []string{"skills", "trust"}, nil); code != 0 || !strings.HasPrefix(out, "Trusted: "+root+"\n2 project skill(s)") || !trusted() {
		t.Fatalf("%d %q", code, out)
	}
}

func TestCLISkillsAndBundlesAcceptArgparseAbbreviations(t *testing.T) {
	c := NewCLI()
	search := c.find("skills search")
	if err := search.ParseFlags([]string{"--lim", "3", "--sou=github", "--js"}); err != nil {
		t.Fatal(err)
	}
	limit, _ := search.Flags().GetInt("limit")
	source, _ := search.Flags().GetString("source")
	asJSON, _ := search.Flags().GetBool("json")
	if limit != 3 || source != "github" || !asJSON {
		t.Fatal(limit, source, asJSON)
	}
	create := c.find("bundles create")
	if err := create.ParseFlags([]string{"--sk", "a", "--skill", "b", "--desc", "d", "--inst", "i", "--fo"}); err != nil {
		t.Fatal(err)
	}
	skillsFlag, _ := create.Flags().GetStringArray("skill")
	desc, _ := create.Flags().GetString("description")
	instr, _ := create.Flags().GetString("instruction")
	force, _ := create.Flags().GetBool("force")
	if strings.Join(skillsFlag, ",") != "a,b" || desc != "d" || instr != "i" || !force {
		t.Fatal(skillsFlag, desc, instr, force)
	}
	if err := c.find("skills browse").ParseFlags([]string{"--s", "5"}); err == nil {
		t.Fatal("ambiguous --s matched one of --source and --size")
	}
	dir := t.TempDir()
	if code, out := runBundlesMain(t, dir, []string{"bundles", "create", "abbr", "--sk", "one", "--desc", "Short"}, nil); code != 0 || !strings.Contains(out, "(loads 1 skills)") {
		t.Fatalf("%d %q", code, out)
	}
}
