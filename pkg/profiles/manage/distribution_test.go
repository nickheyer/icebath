package manage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
)

type cronImportCall struct {
	home, reason string
	ids          []string
}

// Profile env plus a cron store fixture recording every import the installer hands it
func distEnv(t *testing.T) (string, *[]cronImportCall) {
	t.Helper()
	tmp := manageEnv(t)
	calls := &[]cronImportCall{}
	h := &profiles.CronDistributionHooks
	old := *h
	t.Cleanup(func() { *h = old })
	h.LoadJobs = func(home string) ([]any, error) {
		data, err := os.ReadFile(filepath.Join(home, "cron", "jobs.json"))
		if err != nil {
			return nil, err
		}
		var store map[string]any
		if err := json.Unmarshal(data, &store); err != nil {
			return nil, errors.New("corrupt cron store")
		}
		jobs, _ := store["jobs"].([]any)
		return jobs, nil
	}
	h.ImportJobDefinitions = func(home string, shipped *pyjson.Object, reason string) error {
		*calls = append(*calls, cronImportCall{home, reason, append([]string(nil), shipped.Keys...)})
		jobs := []any{}
		for _, k := range shipped.Keys {
			v, _ := shipped.Get(k)
			jobs = append(jobs, v)
		}
		data, _ := json.Marshal(map[string]any{"jobs": jobs})
		return os.WriteFile(filepath.Join(home, "cron", "jobs.json"), data, 0o644)
	}
	return tmp, calls
}

// Local distribution source with SOUL.md, config, mcp.json, one skill, one cron job and the manifest
func makeStagingDir(t *testing.T, root, name string, m *profiles.DistributionManifest) string {
	t.Helper()
	staged := filepath.Join(root, "staging_"+name)
	writeFile(t, filepath.Join(staged, "SOUL.md"), "I am Source.\n")
	writeFile(t, filepath.Join(staged, "config.yaml"), "model:\n  model: gpt-4\n")
	writeFile(t, filepath.Join(staged, "mcp.json"), "{\"servers\": {}}\n")
	writeFile(t, filepath.Join(staged, "skills", "demo", "SKILL.md"), "---\nname: demo\ndescription: test\n---\n# Demo skill\n")
	writeFile(t, filepath.Join(staged, "cron", "jobs.json"), `{"jobs": [{"id": "a1b2c3d4e5f6", "name": "daily", "prompt": "daily task", `+
		`"schedule": {"kind": "cron", "expr": "0 9 * * *", "display": "0 9 * * *"}, "enabled": true, "state": "scheduled"}]}`)
	if m == nil {
		m = profiles.NewDistributionManifest(name)
	}
	if _, err := profiles.WriteManifest(staged, m); err != nil {
		t.Fatal(err)
	}
	return staged
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func mustInstall(t *testing.T, staged, name string, force bool) *InstallPlan {
	t.Helper()
	plan, err := InstallDistribution(context.Background(), staged, name, force, false)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func wantDistError(t *testing.T, err error, sub string) {
	t.Helper()
	var de *profiles.DistributionError
	if !errors.As(err, &de) || !strings.Contains(de.Msg, sub) {
		t.Fatalf("want DistributionError containing %q, got %v", sub, err)
	}
}

func TestManifestParsingFullManifest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, profiles.ManifestFilename), "name: telem\nversion: 1.2.3\ndescription: Telem monitor\n"+
		"hermes_requires: '>=0.12.0'\nauthor: Kyle\nlicense: MIT\nenv_requires:\n  - name: OPENAI_API_KEY\n    description: OpenAI key\n"+
		"  - name: GRAPH_URL\n    required: false\n    default: http://127.0.0.1:8000\ndistribution_owned:\n  - SOUL.md\n  - skills/\n")
	m, err := profiles.ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "telem" || m.Version != "1.2.3" || m.Author != "Kyle" || m.License != "MIT" || len(m.EnvRequires) != 2 {
		t.Fatalf("%+v", m)
	}
	if m.EnvRequires[0].Name != "OPENAI_API_KEY" || !m.EnvRequires[0].Required || m.EnvRequires[1].Required {
		t.Fatalf("%+v", m.EnvRequires)
	}
	if m.EnvRequires[1].Default != "http://127.0.0.1:8000" || !slices.Equal(m.DistributionOwned, []string{"SOUL.md", "skills"}) {
		t.Fatalf("%+v", m)
	}
}

func TestManifestRoundtripWriteRead(t *testing.T) {
	dir := t.TempDir()
	m := &profiles.DistributionManifest{Name: "rt", Version: "1.0.0", Description: "roundtrip",
		EnvRequires: []profiles.EnvRequirement{profiles.NewEnvRequirement("FOO", "foo")}}
	if _, err := profiles.WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	parsed, err := profiles.ReadManifest(dir)
	if err != nil || parsed.Name != "rt" || parsed.EnvRequires[0].Name != "FOO" {
		t.Fatalf("%+v %v", parsed, err)
	}
	if got := readText(t, filepath.Join(dir, profiles.ManifestFilename)); got != "name: rt\nversion: 1.0.0\ndescription: roundtrip\nenv_requires:\n  - name: FOO\n    description: foo\n" {
		t.Fatalf("%q", got)
	}
}

func TestManifestRejectsMalformedShapes(t *testing.T) {
	for body, want := range map[string]string{
		"- a\n":                            "distribution.yaml must be a mapping, got list",
		"version: 1\n":                     "distribution.yaml missing 'name'",
		"name: x\nenv_requires: 3\n":       "env_requires must be a list",
		"name: x\nenv_requires: [3]\n":     "env_requires entry must be a mapping, got int",
		"name: x\nenv_requires: [{}]\n":    "env_requires entry missing 'name'",
		"name: x\ndistribution_owned: a\n": "distribution_owned must be a list",
		"name: [a\n":                       "Failed to parse ",
	} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, profiles.ManifestFilename), body)
		_, err := profiles.ReadManifest(dir)
		wantDistError(t, err, want)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, profiles.ManifestFilename), "\xef\xbb\xbfname: bom\n")
	if m, err := profiles.ReadManifest(dir); err != nil || m.Name != "bom" {
		t.Fatalf("%+v %v", m, err)
	}
	writeFile(t, filepath.Join(dir, profiles.ManifestFilename), "name: \xff\n")
	_, err := profiles.ReadManifest(dir)
	wantDistError(t, err, "'utf-8' codec can't decode byte 0xff in position 6: invalid start byte")
	if m, err := profiles.ReadManifest(t.TempDir()); m != nil || err != nil {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestVersionRequiresMatrix(t *testing.T) {
	cases := []struct {
		spec, cur string
		ok        bool
	}{
		{"", "0.1.0", true}, {">=0.12.0", "0.12.0", true}, {">=0.12.0", "0.13.0", true}, {">=0.12.0", "0.11.9", false},
		{"==0.12.0", "0.12.0", true}, {"==0.12.0", "0.13.0", false}, {"!=0.12.0", "0.13.0", true}, {">0.12.0", "0.12.1", true},
		{">0.12.0", "0.12.0", false}, {"<0.13.0", "0.12.9", true}, {"<=0.12.0", "0.12.0", true}, {"0.12.0", "0.13.0", true},
		{"0.12.0", "0.11.0", false},
	}
	for _, c := range cases {
		err := CheckHermesRequires(c.spec, c.cur)
		if c.ok && err != nil {
			t.Fatalf("%v: %v", c, err)
		}
		if !c.ok {
			wantDistError(t, err, "requires Icebath")
		}
	}
	wantDistError(t, CheckHermesRequires(">=1.0", "unknown"), "Unparseable version: 'unknown'")
}

func TestParseSemverHandlesPrerelease(t *testing.T) {
	for _, v := range []string{"0.12.0-rc1", "v0.12.0+abc"} {
		got, err := parseSemver(v)
		if err != nil || got[0].Int64() != 0 || got[1].Int64() != 12 || got[2].Int64() != 0 {
			t.Fatalf("%s: %v %v", v, got, err)
		}
	}
}

func TestEnvTemplateRequiredIsUncommented(t *testing.T) {
	m := &profiles.DistributionManifest{Name: "x", EnvRequires: []profiles.EnvRequirement{profiles.NewEnvRequirement("FOO", "foo key"),
		{Name: "BAR", Required: false, Default: 8000}}}
	out := envTemplateFromManifest(m)
	for _, want := range []string{"# foo key", "# (required)", "\nFOO=\n", "# (optional)\n# BAR=8000\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q missing %q", out, want)
		}
	}
}

func TestLooksLikeGitURLAcceptsGitSources(t *testing.T) {
	for _, s := range []string{"github.com/user/repo", "https://github.com/user/repo", "https://github.com/user/repo.git",
		"http://example.com/repo", "git@github.com:user/repo.git", "ssh://git@example.com/repo.git", "git://example.com/repo.git"} {
		if !looksLikeGitURL(s) {
			t.Fatal(s)
		}
	}
	if looksLikeGitURL("./local/dir") {
		t.Fatal("local dir read as git")
	}
}

func TestInstallFromDirectory(t *testing.T) {
	tmp, calls := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", nil)
	plan := mustInstall(t, staged, "installed", false)
	if readText(t, filepath.Join(plan.TargetDir, "SOUL.md")) != "I am Source.\n" ||
		!exists(filepath.Join(plan.TargetDir, "skills", "demo", "SKILL.md")) || !exists(filepath.Join(plan.TargetDir, "mcp.json")) {
		t.Fatal("payload missing")
	}
	m, err := profiles.ReadManifest(plan.TargetDir)
	if err != nil || m.Name != "installed" || m.Source != staged {
		t.Fatalf("%+v %v", m, err)
	}
	if len(*calls) != 1 || (*calls)[0].home != plan.TargetDir || !slices.Equal((*calls)[0].ids, []string{"a1b2c3d4e5f6"}) ||
		(*calls)[0].reason != "Installed from a profile distribution; review it, then resume." {
		t.Fatalf("%+v", *calls)
	}
}

func TestInstallRespectsDistributionOwnedAllowlist(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "restricted", &profiles.DistributionManifest{Name: "restricted", Version: "0.1.0",
		DistributionOwned: []string{"SOUL.md", "skills"}})
	plan := mustInstall(t, staged, "restricted", false)
	if readText(t, filepath.Join(plan.TargetDir, "SOUL.md")) != "I am Source.\n" || !exists(filepath.Join(plan.TargetDir, "skills", "demo", "SKILL.md")) {
		t.Fatal("allowlisted payload missing")
	}
	if exists(filepath.Join(plan.TargetDir, "mcp.json")) {
		t.Fatal("mcp.json should not be copied")
	}
	if entries, _ := os.ReadDir(filepath.Join(plan.TargetDir, "cron")); len(entries) != 0 {
		t.Fatalf("cron should be empty: %v", entries)
	}
	if !exists(filepath.Join(plan.TargetDir, "distribution.yaml")) {
		t.Fatal("manifest missing")
	}
}

func TestInstallOmittedAllowlistCopiesEverything(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "legacy_all", nil)
	writeFile(t, filepath.Join(staged, "extra.txt"), "bonus\n")
	writeFile(t, filepath.Join(staged, "assets", "helper.py"), "# helper\n")
	plan := mustInstall(t, staged, "legacy_all", false)
	if readText(t, filepath.Join(plan.TargetDir, "extra.txt")) != "bonus\n" || !exists(filepath.Join(plan.TargetDir, "assets", "helper.py")) {
		t.Fatal("undeclared payload dropped")
	}
}

func TestInstallAllowlistSupportsNestedPaths(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "nested", &profiles.DistributionManifest{Name: "nested", Version: "0.1.0",
		DistributionOwned: []string{"SOUL.md", "skills/research/", "cron/jobs.json"}})
	writeFile(t, filepath.Join(staged, "skills", "research", "SKILL.md"), "---\nname: research\ndescription: r\n---\n# R\n")
	plan := mustInstall(t, staged, "nested", false)
	if !exists(filepath.Join(plan.TargetDir, "skills", "research", "SKILL.md")) || !exists(filepath.Join(plan.TargetDir, "cron", "jobs.json")) {
		t.Fatal("nested allowlist entries missing")
	}
	if exists(filepath.Join(plan.TargetDir, "skills", "demo")) || exists(filepath.Join(plan.TargetDir, "mcp.json")) {
		t.Fatal("unlisted entries copied")
	}
}

func TestUpdateRespectsDistributionOwnedAllowlist(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "up_src", nil)
	plan := mustInstall(t, staged, "up_restricted", false)
	if !exists(filepath.Join(plan.TargetDir, "mcp.json")) {
		t.Fatal("baseline mcp.json missing")
	}
	if _, err := profiles.WriteManifest(staged, &profiles.DistributionManifest{Name: "up_restricted", Version: "0.2.0",
		DistributionOwned: []string{"SOUL.md", "skills"}}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(staged, "new_config.toml"), "[extra]\n")
	onDisk, _ := profiles.ReadManifest(plan.TargetDir)
	onDisk.Source = staged
	if _, err := profiles.WriteManifest(plan.TargetDir, onDisk); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateDistribution(context.Background(), "up_restricted", true); err != nil {
		t.Fatal(err)
	}
	if readText(t, filepath.Join(plan.TargetDir, "SOUL.md")) != "I am Source.\n" || exists(filepath.Join(plan.TargetDir, "new_config.toml")) {
		t.Fatal("update ignored the allowlist")
	}
}

func TestInstallRejectsNonDistributionDirectory(t *testing.T) {
	tmp, _ := distEnv(t)
	bogus := filepath.Join(tmp, "bogus_dir")
	writeFile(t, filepath.Join(bogus, "some_file"), "hi")
	_, err := PlanInstall(context.Background(), bogus, filepath.Join(tmp, "work"), "x")
	wantDistError(t, err, "No distribution.yaml")
	_, err = PlanInstall(context.Background(), filepath.Join(tmp, "nowhere"), tmp, "x")
	wantDistError(t, err, "Cannot resolve distribution source: '"+filepath.Join(tmp, "nowhere")+"'. Expected a git URL")
}

func TestInstallEnforcesHermesRequires(t *testing.T) {
	tmp, _ := distEnv(t)
	old := distBaseVersion
	distBaseVersion = func() string { return "0.1.0" }
	t.Cleanup(func() { distBaseVersion = old })
	staged := makeStagingDir(t, tmp, "future", &profiles.DistributionManifest{Name: "future", Version: "1.0.0", HermesRequires: ">=99.0.0"})
	_, err := InstallDistribution(context.Background(), staged, "future", false, false)
	wantDistError(t, err, "requires Icebath")
}

func TestInstallRefusesDefaultAndExistingWithoutForce(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", nil)
	_, err := InstallDistribution(context.Background(), staged, "default", false, false)
	wantDistError(t, err, "Cannot install a distribution as 'default'")
	mustInstall(t, staged, "twice", false)
	_, err = InstallDistribution(context.Background(), staged, "twice", false, false)
	wantDistError(t, err, "Profile 'twice' already exists at ")
}

func TestUpdateAndForceInstallMergeOwnedDirsPerRoot(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", nil)
	plan := mustInstall(t, staged, "skills_safe", false)
	custom := filepath.Join(plan.TargetDir, "skills", "custom")
	writeFile(t, filepath.Join(custom, "SKILL.md"), "custom skill\n")
	writeFile(t, filepath.Join(plan.TargetDir, "cron", "mine.json"), "{\"schedule\": \"* * * * *\"}\n")
	writeFile(t, filepath.Join(plan.TargetDir, "skills", "demo", "stale.txt"), "old file\n")
	writeFile(t, filepath.Join(staged, "skills", "demo", "SKILL.md"), "updated demo\n")
	writeFile(t, filepath.Join(staged, "skills", "new", "SKILL.md"), "new skill\n")
	writeFile(t, filepath.Join(staged, "skills", "devops", "team-deploy", "SKILL.md"), "team deploy\n")
	mine := filepath.Join(plan.TargetDir, "skills", "devops", "my-custom-skill")
	writeFile(t, filepath.Join(mine, "SKILL.md"), "my custom skill\n")
	if _, err := UpdateDistribution(context.Background(), "skills_safe", false); err != nil {
		t.Fatal(err)
	}
	if readText(t, filepath.Join(custom, "SKILL.md")) != "custom skill\n" || readText(t, filepath.Join(mine, "SKILL.md")) != "my custom skill\n" {
		t.Fatal("installer roots lost")
	}
	if !exists(filepath.Join(plan.TargetDir, "skills", "devops", "team-deploy", "SKILL.md")) ||
		readText(t, filepath.Join(plan.TargetDir, "cron", "mine.json")) != "{\"schedule\": \"* * * * *\"}\n" ||
		readText(t, filepath.Join(plan.TargetDir, "skills", "demo", "SKILL.md")) != "updated demo\n" ||
		readText(t, filepath.Join(plan.TargetDir, "skills", "new", "SKILL.md")) != "new skill\n" ||
		exists(filepath.Join(plan.TargetDir, "skills", "demo", "stale.txt")) {
		t.Fatal("update did not merge per root")
	}
	mustInstall(t, staged, "skills_safe", true)
	if readText(t, filepath.Join(custom, "SKILL.md")) != "custom skill\n" || !exists(filepath.Join(plan.TargetDir, "cron", "mine.json")) {
		t.Fatal("force install lost installer roots")
	}
}

func ownedCategory(t *testing.T, tmp, name string) (string, *InstallPlan) {
	t.Helper()
	staged := makeStagingDir(t, tmp, name, &profiles.DistributionManifest{Name: name, Version: "0.1.0",
		DistributionOwned: []string{"SOUL.md", "skills/research/"}})
	writeFile(t, filepath.Join(staged, "skills", "research", "web-search", "SKILL.md"), "author skill\n")
	writeFile(t, filepath.Join(staged, "skills", "research", "DESCRIPTION.md"), "research skills\n")
	writeFile(t, filepath.Join(staged, "skills", "research", ".DS_Store"), "\x00")
	return staged, mustInstall(t, staged, name, false)
}

func TestAnOwnedCategoryKeepsSkillsTheInstallerAddedToIt(t *testing.T) {
	tmp, _ := distEnv(t)
	staged, plan := ownedCategory(t, tmp, "rb")
	research := filepath.Join(plan.TargetDir, "skills", "research")
	writeFile(t, filepath.Join(research, "my-notes", "SKILL.md"), "my own skill\n")
	writeFile(t, filepath.Join(research, "web-search", "stale.txt"), "old\n")
	writeFile(t, filepath.Join(staged, "skills", "research", "web-search", "SKILL.md"), "author v2\n")
	writeFile(t, filepath.Join(staged, "skills", "research", "arxiv", "SKILL.md"), "new author skill\n")
	writeFile(t, filepath.Join(staged, "skills", "research", "README.md"), "about research\n")
	if _, err := UpdateDistribution(context.Background(), "rb", false); err != nil {
		t.Fatal(err)
	}
	if readText(t, filepath.Join(research, "my-notes", "SKILL.md")) != "my own skill\n" ||
		readText(t, filepath.Join(research, "web-search", "SKILL.md")) != "author v2\n" ||
		exists(filepath.Join(research, "web-search", "stale.txt")) || !exists(filepath.Join(research, "arxiv", "SKILL.md")) ||
		readText(t, filepath.Join(research, "DESCRIPTION.md")) != "research skills\n" ||
		readText(t, filepath.Join(research, "README.md")) != "about research\n" {
		t.Fatal("owned category not merged per root")
	}
	scripts := filepath.Join(staged, "skills", "research", "web-search", "scripts")
	if err := os.Mkdir(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	if merge, err := mergesPerRoot(scripts, []string{"skills", "research", "web-search", "scripts"}); merge || err != nil {
		t.Fatal("a dir inside a skill merged per root")
	}
}

func TestAnOwnedCategoryRefusesASymlinkedSubcategoryBeforeWriting(t *testing.T) {
	tmp, _ := distEnv(t)
	staged, plan := ownedCategory(t, tmp, "rb")
	writeFile(t, filepath.Join(staged, "skills", "research", "papers", "summarize", "SKILL.md"), "s\n")
	outside := filepath.Join(tmp, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(plan.TargetDir, "skills", "research", "papers")); err != nil {
		t.Skip("symlinks unavailable in test environment: " + err.Error())
	}
	soul := filepath.Join(plan.TargetDir, "SOUL.md")
	before := readText(t, soul)
	writeFile(t, filepath.Join(staged, "SOUL.md"), "changed\n")
	_, err := UpdateDistribution(context.Background(), "rb", false)
	wantDistError(t, err, "symlink")
	if entries, _ := os.ReadDir(outside); len(entries) != 0 || readText(t, soul) != before {
		t.Fatal("write happened before the refusal")
	}
}

func snapshotFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			out[p] = readText(t, p)
		}
		return nil
	})
	return out
}

func TestUpdateRefusesSymlinkedOwnedContainer(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", nil)
	plan := mustInstall(t, staged, "link_safe", false)
	shared := filepath.Join(tmp, "shared-skills")
	writeFile(t, filepath.Join(shared, "mine", "SKILL.md"), "shared skill\n")
	before := snapshotFiles(t, shared)
	writeFile(t, filepath.Join(staged, "skills", "devops", "team-deploy", "SKILL.md"), "team deploy\n")
	devops := filepath.Join(plan.TargetDir, "skills", "devops")
	if err := os.Symlink(shared, devops); err != nil {
		t.Skip("symlinks unavailable in test environment: " + err.Error())
	}
	_, err := UpdateDistribution(context.Background(), "link_safe", false)
	wantDistError(t, err, "symlink")
	if !isSymlink(devops) {
		t.Fatal("symlink replaced")
	}
	os.Remove(devops)
	skillsDir := filepath.Join(plan.TargetDir, "skills")
	if err := os.RemoveAll(skillsDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, skillsDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(staged, "skills", "demo", "SKILL.md"), "updated demo\n")
	writeFile(t, filepath.Join(staged, "SOUL.md"), "updated soul\n")
	writeFile(t, filepath.Join(staged, "mcp.json"), "{\"servers\": {\"new\": {}}}\n")
	writeFile(t, filepath.Join(staged, "cron", "daily.json"), "{\"schedule\": \"0 10 * * *\"}")
	untouched := snapshotFiles(t, plan.TargetDir)
	_, err = UpdateDistribution(context.Background(), "link_safe", false)
	wantDistError(t, err, "symlink")
	if !isSymlink(skillsDir) || resolvePath(skillsDir) != resolvePath(shared) {
		t.Fatal("skills link replaced")
	}
	after := snapshotFiles(t, shared)
	if len(after) != len(before) || after[filepath.Join(shared, "mine", "SKILL.md")] != "shared skill\n" {
		t.Fatalf("shared tree changed: %v", after)
	}
	got := snapshotFiles(t, plan.TargetDir)
	if len(got) != len(untouched) {
		t.Fatalf("profile changed: %v", got)
	}
	for k, v := range untouched {
		if got[k] != v {
			t.Fatalf("%s changed", k)
		}
	}
}

func TestUpdatePreservesUserData(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", nil)
	plan := mustInstall(t, staged, "telem", false)
	writeFile(t, filepath.Join(plan.TargetDir, "memories", "MEMORY.md"), "# USER MEMORY\n")
	writeFile(t, filepath.Join(plan.TargetDir, ".env"), "OPENAI_API_KEY=sk-user\n")
	writeFile(t, filepath.Join(plan.TargetDir, "auth.json"), `{"user": "auth"}`)
	writeFile(t, filepath.Join(plan.TargetDir, "sessions", "chat.json"), `{"s": 1}`)
	writeFile(t, filepath.Join(staged, "SOUL.md"), "I am Source v2.\n")
	if _, err := UpdateDistribution(context.Background(), "telem", false); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{"SOUL.md": "I am Source v2.\n", "memories/MEMORY.md": "# USER MEMORY\n",
		".env": "OPENAI_API_KEY=sk-user\n", "auth.json": `{"user": "auth"}`, "sessions/chat.json": `{"s": 1}`} {
		if got := readText(t, filepath.Join(plan.TargetDir, rel)); got != want {
			t.Fatalf("%s = %q", rel, got)
		}
	}
}

func TestUpdatePreservesConfigByDefault(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", nil)
	plan := mustInstall(t, staged, "t2", false)
	writeFile(t, filepath.Join(plan.TargetDir, "config.yaml"), "model:\n  model: gpt-5\n# user override\n")
	writeFile(t, filepath.Join(staged, "config.yaml"), "model:\n  model: claude\n")
	if _, err := UpdateDistribution(context.Background(), "t2", false); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, filepath.Join(plan.TargetDir, "config.yaml")); !strings.Contains(got, "gpt-5") || !strings.Contains(got, "user override") {
		t.Fatal(got)
	}
}

func TestUpdateMissingManifestErrors(t *testing.T) {
	distEnv(t)
	if _, err := CreateProfile(context.Background(), "plain", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := UpdateDistribution(context.Background(), "plain", false)
	wantDistError(t, err, "not a distribution")
	dir, _ := profiles.GetProfileDir("plain")
	if _, err := profiles.WriteManifest(dir, profiles.NewDistributionManifest("plain")); err != nil {
		t.Fatal(err)
	}
	_, err = UpdateDistribution(context.Background(), "plain", false)
	wantDistError(t, err, "Profile 'plain' has no recorded source.  Re-install with `icebath profile install <source> --name {canon} --force`.")
}

func TestDescribeExistingDistribution(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "telem", &profiles.DistributionManifest{Name: "telem", Version: "1.0.0", Description: "compliance monitor",
		EnvRequires: []profiles.EnvRequirement{profiles.NewEnvRequirement("API", "api key")}})
	mustInstall(t, staged, "telem", false)
	data, err := DescribeDistribution("telem")
	if err != nil {
		t.Fatal(err)
	}
	env := data["env_requires"].([]any)
	if data["name"] != "telem" || data["version"] != "1.0.0" || env[0].(map[string]any)["name"] != "API" {
		t.Fatalf("%v", data)
	}
	if !slices.Equal(config.MappingKeys(data), []string{"name", "version", "description", "env_requires", "source", "installed_at"}) {
		t.Fatalf("%v", config.MappingKeys(data))
	}
}

func TestDescribeMissingProfileRaises(t *testing.T) {
	distEnv(t)
	_, err := DescribeDistribution("nonexistent")
	wantDistError(t, err, "No profile named")
	if !strings.Contains(err.Error(), "icebath profile list") {
		t.Fatal(err)
	}
}

func TestInstallDoesNotImportCredentialsFromStaging(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", nil)
	writeFile(t, filepath.Join(staged, "auth.json"), `{"leaked": true}`)
	writeFile(t, filepath.Join(staged, ".env"), "LEAKED=1")
	plan := mustInstall(t, staged, "clean", false)
	if exists(filepath.Join(plan.TargetDir, "auth.json")) {
		t.Fatal("auth.json leaked")
	}
	if exists(filepath.Join(plan.TargetDir, ".env")) && strings.Contains(readText(t, filepath.Join(plan.TargetDir, ".env")), "LEAKED") {
		t.Fatal(".env leaked")
	}
}

func TestInstallRejectsSymlinkedDistributionFiles(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", nil)
	secret := filepath.Join(tmp, "local-secret.txt")
	writeFile(t, secret, "outside secret\n")
	if err := os.Symlink(secret, filepath.Join(staged, "skills", "demo", "leak.txt")); err != nil {
		t.Skip("symlinks unavailable in test environment: " + err.Error())
	}
	_, err := InstallDistribution(context.Background(), staged, "clean", false, false)
	wantDistError(t, err, "Profile distributions cannot contain symlinks: "+filepath.Join("skills", "demo", "leak.txt"))
	target, _ := profiles.GetProfileDir("clean")
	if exists(filepath.Join(target, "skills", "demo", "leak.txt")) {
		t.Fatal("symlink copied")
	}
}

func TestNestedUserOwnedExcludeNotFiltered(t *testing.T) {
	tmp, _ := distEnv(t)
	owned := append(slices.Clone(DefaultDistOwned), "assets")
	staged := makeStagingDir(t, tmp, "src", &profiles.DistributionManifest{Name: "nested_bin", Version: "0.1.0", DistributionOwned: owned})
	writeFile(t, filepath.Join(staged, "assets", "bin", "tool.py"), "# tool\n")
	plan := mustInstall(t, staged, "nested_bin", false)
	if !exists(filepath.Join(plan.TargetDir, "assets", "bin", "tool.py")) {
		t.Fatal("nested bin dropped")
	}

	staged = makeStagingDir(t, tmp, "top", nil)
	writeFile(t, filepath.Join(staged, "bin", "shipped_binary"), "x")
	writeFile(t, filepath.Join(staged, "logs", "shipped.log"), "y\n")
	plan = mustInstall(t, staged, "top_filter", false)
	if exists(filepath.Join(plan.TargetDir, "bin")) || exists(filepath.Join(plan.TargetDir, "logs", "shipped.log")) {
		t.Fatal("top level user owned entries copied")
	}

	staged = makeStagingDir(t, tmp, "coexist", &profiles.DistributionManifest{Name: "coexist", Version: "0.1.0", DistributionOwned: owned})
	writeFile(t, filepath.Join(staged, "bin", "top.sh"), "# top\n")
	writeFile(t, filepath.Join(staged, "assets", "bin", "helper.py"), "# helper\n")
	plan = mustInstall(t, staged, "coexist", false)
	if exists(filepath.Join(plan.TargetDir, "bin")) || !exists(filepath.Join(plan.TargetDir, "assets", "bin", "helper.py")) {
		t.Fatal("nested and top level filtering mixed up")
	}
}

func TestInstallStampsAndUpdateRefreshesInstalledAt(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", nil)
	plan := mustInstall(t, staged, "demo", false)
	first, _ := profiles.ReadManifest(plan.TargetDir)
	at := first.InstalledAt
	if len(at) < 4 || !strings.Contains(at, "T") || !strings.HasSuffix(at, "+00:00") || strings.Trim(at[:4], "0123456789") != "" {
		t.Fatal(at)
	}
	old := distNow
	distNow = func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { distNow = old })
	if _, err := UpdateDistribution(context.Background(), "demo", false); err != nil {
		t.Fatal(err)
	}
	refreshed, _ := profiles.ReadManifest(plan.TargetDir)
	if refreshed.InstalledAt != "2099-01-01T00:00:00+00:00" {
		t.Fatal(refreshed.InstalledAt)
	}
}

func TestInstalledDistributionShowsInList(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", &profiles.DistributionManifest{Name: "telem", Version: "1.2.3"})
	mustInstall(t, staged, "telem", false)
	if _, err := CreateProfile(context.Background(), "brokenmeta", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	broken, _ := profiles.GetProfileDir("brokenmeta")
	writeFile(t, filepath.Join(broken, "distribution.yaml"), "not: [a, valid, mapping\n")
	rows, err := ListProfiles(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range rows {
		switch r.Name {
		case "telem":
			seen++
			if r.DistributionName != "telem" || r.DistributionVersion != "1.2.3" || r.DistributionSource == nil {
				t.Fatalf("%+v", r)
			}
		case "brokenmeta":
			seen++
			if r.DistributionName != nil {
				t.Fatalf("%+v", r)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("%+v", rows)
	}
}

func TestBadProfileNameRaisesValueError(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "bad", &profiles.DistributionManifest{Name: "Invalid Name With Spaces", Version: "0.1.0"})
	_, err := PlanInstall(context.Background(), staged, filepath.Join(tmp, "work"), "")
	var ve *profiles.ValueError
	var de *profiles.DistributionError
	if !errors.As(err, &ve) && !errors.As(err, &de) {
		t.Fatalf("%T %v", err, err)
	}
}

func TestPreviousManifestSurvivesAnInterruptedWrite(t *testing.T) {
	dir := t.TempDir()
	original := &profiles.DistributionManifest{Name: "keepme", Version: "1.0.0", Description: "the manifest already on disk",
		EnvRequires: []profiles.EnvRequirement{profiles.NewEnvRequirement("FOO", "foo")}}
	if _, err := profiles.WriteManifest(dir, original); err != nil {
		t.Fatal(err)
	}
	onDisk := readText(t, filepath.Join(dir, "distribution.yaml"))
	restore := config.SwapSafeDump(func(any) ([]byte, error) { return nil, errors.New("simulated crash mid-write") })
	_, err := profiles.WriteManifest(dir, &profiles.DistributionManifest{Name: "replacement", Version: "2.0.0"})
	restore()
	if err == nil {
		t.Fatal("interrupted write reported success")
	}
	if readText(t, filepath.Join(dir, "distribution.yaml")) != onDisk {
		t.Fatal("manifest changed")
	}
	parsed, err := profiles.ReadManifest(dir)
	if err != nil || parsed.Name != "keepme" || parsed.EnvRequires[0].Name != "FOO" {
		t.Fatalf("%+v %v", parsed, err)
	}
	if tmps, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(tmps) != 0 {
		t.Fatal(tmps)
	}
}

func TestManifestFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	dir := t.TempDir()
	mf := filepath.Join(dir, "distribution.yaml")
	if _, err := profiles.WriteManifest(dir, &profiles.DistributionManifest{Name: "fresh", Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(mf); st.Mode().Perm() != 0o644 {
		t.Fatalf("new manifest created as %o", st.Mode().Perm())
	}
	if err := os.Chmod(mf, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := profiles.WriteManifest(dir, &profiles.DistributionManifest{Name: "modes", Version: "2.0.0"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(mf); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %o", st.Mode().Perm())
	}
}

func TestCronMergeErrorsBecomeDistributionErrors(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", nil)
	h := &profiles.CronDistributionHooks
	h.ImportJobDefinitions = func(string, *pyjson.Object, string) error { return errors.New("cron job 'daily': bad schedule") }
	_, err := InstallDistribution(context.Background(), staged, "cronbad", false, false)
	wantDistError(t, err, "Could not merge cron jobs into ")
	if !strings.Contains(err.Error(), "cron job 'daily': bad schedule") {
		t.Fatal(err)
	}
	target, _ := profiles.GetProfileDir("cronbad")
	if exists(filepath.Join(target, "SOUL.md")) {
		t.Fatal("payload written after a refused cron merge")
	}
	h.ImportJobDefinitions = func(string, *pyjson.Object, string) error {
		return &os.PathError{Op: "open", Path: "x", Err: os.ErrPermission}
	}
	_, err = InstallDistribution(context.Background(), staged, "cronbad", true, false)
	var pe *os.PathError
	var de *profiles.DistributionError
	if !errors.As(err, &pe) || errors.As(err, &de) {
		t.Fatalf("OS error was wrapped: %v", err)
	}
}
