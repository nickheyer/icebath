package dashboard

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/skills"
)

func editorSkillMD(name string) string {
	return "---\nname: " + name + "\ndescription: a test skill\n---\n\n# " + name + "\n\nDo the thing.\n"
}

// Writes a skill directory holding a SKILL.md with the given text
func writeSkillText(t *testing.T, skillsDir, dir, text string) {
	t.Helper()
	writeTestFile(t, filepath.Join(skillsDir, dir, "SKILL.md"), text)
}

// Default home with dashboard-skill and the profile worker_alpha with worker-skill, each built by build
func skillProfiles(t *testing.T, build func(name string) string) (sessionClient, string, string) {
	t.Helper()
	c := newSessionClient(t)
	defaultHome := os.Getenv("ICEBATH_HOME")
	workerHome := filepath.Join(defaultHome, "profiles", "worker_alpha")
	for _, h := range []string{defaultHome, workerHome} {
		if err := os.MkdirAll(filepath.Join(h, "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(h, "config.yaml"), "{}\n")
	}
	writeSkillText(t, filepath.Join(defaultHome, "skills"), "dashboard-skill", build("dashboard-skill"))
	writeSkillText(t, filepath.Join(workerHome, "skills"), "worker-skill", build("worker-skill"))
	return c, defaultHome, workerHome
}

func editorProfiles(t *testing.T) (sessionClient, string, string) {
	return skillProfiles(t, editorSkillMD)
}

func profileSkillMD(name string) string {
	return "---\nname: " + name + "\ndescription: A test skill.\n---\n\n# " + name + "\n"
}

func scopedProfiles(t *testing.T) (sessionClient, string, string) {
	return skillProfiles(t, profileSkillMD)
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSkillContentGetContentReturnsRawSkillMd(t *testing.T) {
	c, _, _ := editorProfiles(t)
	resp := c.get("/api/skills/content?name=dashboard-skill")
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	data := jsonObject(t, resp)
	content, _ := data["content"].(string)
	if data["name"] != "dashboard-skill" || !strings.HasPrefix(content, "---") || !strings.Contains(content, "Do the thing.") {
		t.Fatalf("data %v", data)
	}
}

func TestSkillContentGetAndUpdateByFrontmatterDisplayName(t *testing.T) {
	c, defaultHome, _ := editorProfiles(t)
	skillsDir := filepath.Join(defaultHome, "skills")
	writeSkillText(t, skillsDir, "agong-deep-search-trace", editorSkillMD("AGong Deep Search Trace"))
	resp := c.get("/api/skills/content?name=" + url.QueryEscape("AGong Deep Search Trace"))
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	content := strings.ReplaceAll(jsonObject(t, resp)["content"].(string), "Do the thing.", "Do the NEW thing.")
	resp = c.send("PUT", "/api/skills/content", map[string]any{"name": "AGong Deep Search Trace", "content": content})
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	if got := readTestFile(t, filepath.Join(skillsDir, "agong-deep-search-trace", "SKILL.md")); !strings.Contains(got, "Do the NEW thing.") {
		t.Fatalf("SKILL.md %q", got)
	}
}

func TestSkillContentGetContentScopesToProfile(t *testing.T) {
	c, _, _ := editorProfiles(t)
	if resp := c.get("/api/skills/content?name=worker-skill&profile=worker_alpha"); resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	if resp := c.get("/api/skills/content?name=worker-skill"); resp.Status != 404 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}

func TestSkillCreateCreateWritesSkillMd(t *testing.T) {
	c, defaultHome, _ := editorProfiles(t)
	resp := c.send("POST", "/api/skills", map[string]any{"name": "my-new-skill", "content": editorSkillMD("my-new-skill")})
	if resp.Status != 200 || jsonObject(t, resp)["success"] != true {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	if got := readTestFile(t, filepath.Join(defaultHome, "skills", "my-new-skill", "SKILL.md")); !strings.Contains(got, "Do the thing.") {
		t.Fatalf("SKILL.md %q", got)
	}
}

func TestSkillCreateCreateRejectsMissingFrontmatter(t *testing.T) {
	c, defaultHome, _ := editorProfiles(t)
	resp := c.send("POST", "/api/skills", map[string]any{"name": "bad-skill", "content": "no frontmatter here"})
	if resp.Status != 400 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	if detail, _ := jsonObject(t, resp)["detail"].(string); !strings.Contains(strings.ToLower(detail), "frontmatter") {
		t.Fatalf("detail %q", detail)
	}
	if _, err := os.Lstat(filepath.Join(defaultHome, "skills", "bad-skill")); err == nil {
		t.Fatal("refused skill left a directory")
	}
}

func TestSkillCreateCreateRejectsInvalidName(t *testing.T) {
	c, _, _ := editorProfiles(t)
	if resp := c.send("POST", "/api/skills", map[string]any{"name": "../escape", "content": editorSkillMD("x")}); resp.Status != 400 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}

func TestSkillUpdateUpdateRewritesSkillMd(t *testing.T) {
	c, defaultHome, _ := editorProfiles(t)
	content := strings.ReplaceAll(editorSkillMD("dashboard-skill"), "Do the thing.", "Do the NEW thing.")
	if resp := c.send("PUT", "/api/skills/content", map[string]any{"name": "dashboard-skill", "content": content}); resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	if got := readTestFile(t, filepath.Join(defaultHome, "skills", "dashboard-skill", "SKILL.md")); !strings.Contains(got, "Do the NEW thing.") {
		t.Fatalf("SKILL.md %q", got)
	}
}

func TestSkillUpdateUpdateInvalidFrontmatter400(t *testing.T) {
	c, _, _ := editorProfiles(t)
	if resp := c.send("PUT", "/api/skills/content", map[string]any{"name": "dashboard-skill", "content": "broken"}); resp.Status != 400 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}

// Names in the skills.disabled list of a home config on disk
func disabledOnDisk(t *testing.T, h string) []any {
	t.Helper()
	section, _ := rawConfigOnDisk(t, h)["skills"].(map[string]any)
	list, _ := section["disabled"].([]any)
	return list
}

func TestProfileScopedSkillsToggleWritesIntoTargetProfileOnly(t *testing.T) {
	c, defaultHome, workerHome := scopedProfiles(t)
	resp := c.send("PUT", "/api/skills/toggle", map[string]any{"name": "worker-skill", "enabled": false, "profile": "worker_alpha"})
	resp.expect(t, 200, `{"ok":true,"name":"worker-skill","enabled":false}`)
	if got := disabledOnDisk(t, workerHome); !reflect.DeepEqual(got, []any{"worker-skill"}) {
		t.Fatalf("worker disabled %v", got)
	}
	for _, name := range disabledOnDisk(t, defaultHome) {
		if name == "worker-skill" {
			t.Fatal("the dashboard profile took the write")
		}
	}
}

func TestProfileScopedSkillsScopeRestoresModuleGlobals(t *testing.T) {
	c, _, _ := scopedProfiles(t)
	before := home.SkillsDir(home.Dir(context.Background()))
	c.get("/api/skills?profile=worker_alpha")
	if after := home.SkillsDir(home.Dir(context.Background())); after != before {
		t.Fatalf("skills dir moved from %s to %s", before, after)
	}
}

func TestProfileScopedSkillsQueryProfileScopesSkillWrites(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		body         map[string]any
	}{
		{"PUT", "/api/learning/node", map[string]any{"id": "shared-skill", "content": "EDITED"}},
		{"DELETE", "/api/learning/node", map[string]any{"id": "shared-skill"}},
		{"PUT", "/api/skills/content", map[string]any{"name": "shared-skill", "content": "EDITED"}},
		{"POST", "/api/skills", map[string]any{"name": "shared-skill-2", "content": "EDITED"}},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			c, defaultHome, workerHome := scopedProfiles(t)
			target, _ := tc.body["name"].(string)
			if target == "" {
				target, _ = tc.body["id"].(string)
			}
			body := map[string]any{}
			for k, v := range tc.body {
				body[k] = v
			}
			if _, has := body["content"]; has {
				body["content"] = "---\nname: " + target + "\ndescription: edited\n---\n\n# EDITED\n"
			}
			for _, h := range []string{defaultHome, workerHome} {
				writeSkillText(t, filepath.Join(h, "skills"), "shared-skill", profileSkillMD("shared-skill"))
			}
			defaultMD := filepath.Join(defaultHome, "skills", "shared-skill", "SKILL.md")
			before := readTestFile(t, defaultMD)
			resp := c.send(tc.method, tc.path+"?profile=worker_alpha", body)
			if resp.Status != 200 {
				t.Fatalf("status %d %s", resp.Status, resp.Body)
			}
			workerSkills := filepath.Join(workerHome, "skills")
			if tc.method == "DELETE" {
				if _, err := os.Lstat(filepath.Join(workerSkills, "shared-skill")); err == nil {
					t.Fatal("deleted skill still on disk")
				}
			} else if got := readTestFile(t, filepath.Join(workerSkills, target, "SKILL.md")); !strings.Contains(got, "# EDITED") {
				t.Fatalf("SKILL.md %q", got)
			}
			if readTestFile(t, defaultMD) != before {
				t.Fatal("the dashboard profile skill changed")
			}
			if _, err := os.Lstat(filepath.Join(defaultHome, "skills", "shared-skill-2")); err == nil {
				t.Fatal("the dashboard profile took the new skill")
			}
		})
	}
}

// Whether a skill directory carries the lock record in a home
func skillLockedIn(t *testing.T, h, name string) bool {
	t.Helper()
	locked, err := skills.NewScope(h, nil).IsLocked(name)
	if err != nil {
		t.Fatal(err)
	}
	return locked
}

func TestProfileScopedSkillsLockRoundTripsInTargetProfileAndListCarriesIt(t *testing.T) {
	c, defaultHome, workerHome := scopedProfiles(t)
	writeSkillText(t, filepath.Join(workerHome, "skills"), "dir-name", "---\nname: shown-name\ndescription: x\n---\n\n# x\n")
	resp := c.send("PUT", "/api/skills/lock?profile=worker_alpha", map[string]any{"name": "shown-name", "locked": true})
	resp.expect(t, 200, `{"ok":true,"name":"shown-name","locked":true}`)
	if !skillLockedIn(t, workerHome, "dir-name") || skillLockedIn(t, defaultHome, "dir-name") {
		t.Fatalf("lock landed worker %v default %v", skillLockedIn(t, workerHome, "dir-name"), skillLockedIn(t, defaultHome, "dir-name"))
	}
	rows, ok := pyjson.Plain(c.get("/api/skills?profile=worker_alpha").JSON(t)).([]any)
	if !ok {
		t.Fatal("skills listing is not a list")
	}
	got := map[string]any{}
	for _, r := range rows {
		row := r.(map[string]any)
		got[row["name"].(string)] = row["locked"]
	}
	if !reflect.DeepEqual(got, map[string]any{"worker-skill": false, "shown-name": true}) {
		t.Fatalf("locked rows %v", got)
	}
	if resp := c.send("PUT", "/api/skills/lock?profile=worker_alpha", map[string]any{"name": "shown-name", "locked": false}); resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	if skillLockedIn(t, workerHome, "dir-name") {
		t.Fatal("unlock left the record")
	}
}

func TestProfileScopedSkillsLockUnknownSkill404AndCorruptSidecarListsUnlocked(t *testing.T) {
	c, defaultHome, _ := scopedProfiles(t)
	if resp := c.send("PUT", "/api/skills/lock", map[string]any{"name": "ghost", "locked": true}); resp.Status != 404 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	writeTestFile(t, filepath.Join(defaultHome, "skills", ".usage.json"), "{not json")
	resp := c.get("/api/skills")
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	rows := pyjson.Plain(resp.JSON(t)).([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["locked"] != false {
		t.Fatalf("rows %v", rows)
	}
}

func TestProfileScopedHubActionsHubInstallSpawnsWithProfileFlag(t *testing.T) {
	c, _, _ := scopedProfiles(t)
	calls := spawnRecorder(t)
	resp := c.send("POST", "/api/skills/hub/install", map[string]any{"identifier": "official/demo", "profile": "worker_alpha"})
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	want := []spawnCall{{[]string{"-p", "worker_alpha", "skills", "install", "official/demo", "--yes"}, hubActionName("install", "official/demo")}}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls %v, want %v", *calls, want)
	}
}

func TestProfileScopedHubActionsHubInstallUnknownProfile404(t *testing.T) {
	c, _, _ := scopedProfiles(t)
	if resp := c.send("POST", "/api/skills/hub/install", map[string]any{"identifier": "official/demo", "profile": "ghost"}); resp.Status != 404 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}

// Minimal hub source of the monkeypatched source router
type fakeHubSource struct {
	id        string
	available bool
	found     []skills.SkillMeta
}

func (s *fakeHubSource) Search(context.Context, string, int) ([]skills.SkillMeta, error) {
	return s.found, nil
}
func (s *fakeHubSource) Fetch(context.Context, string) (*skills.SkillBundle, error) { return nil, nil }
func (s *fakeHubSource) Inspect(context.Context, string) (*skills.SkillMeta, error) { return nil, nil }
func (s *fakeHubSource) SourceID() string                                           { return s.id }
func (s *fakeHubSource) TrustLevelFor(string) string                                { return "community" }
func (s *fakeHubSource) CurrentRevision(context.Context, string) (string, error)    { return "", nil }
func (s *fakeHubSource) IsAvailable() bool                                          { return s.available }

func fakeHubMeta(identifier, trust, source string) skills.SkillMeta {
	repo := "owner/repo"
	return skills.SkillMeta{Name: identifier[strings.LastIndex(identifier, "/")+1:], Description: "desc", Source: source,
		Identifier: identifier, TrustLevel: trust, Repo: &repo, Tags: []string{"a", "b"}}
}

// Bundle mixing text and byte members, the shape that drives the decode or mask branch of the preview
func fakeHubBundle(identifier string) *skills.SkillBundle {
	return &skills.SkillBundle{Name: identifier[strings.LastIndex(identifier, "/")+1:], Identifier: identifier, Source: "github",
		TrustLevel: "community", Files: []skills.BundleFile{
			{Path: "SKILL.md", Data: []byte("---\nname: x\n---\nbody text"), Binary: true},
			{Path: "icon.png", Data: []byte("\xff\xd8\xff\xe0binary"), Binary: true},
			{Path: "notes.txt", Data: []byte("plain string content")},
		}}
}

func fakeSourceRouter(t *testing.T, sources ...skills.SkillSource) {
	t.Helper()
	real := hubCreateSourceRouter
	hubCreateSourceRouter = func(context.Context) ([]skills.SkillSource, error) { return sources, nil }
	t.Cleanup(func() { hubCreateSourceRouter = real })
}

func fakeHubResolve(t *testing.T, meta *skills.SkillMeta, bundle *skills.SkillBundle) {
	t.Helper()
	real := hubResolveSourceMetaAndBundle
	hubResolveSourceMetaAndBundle = func(context.Context, string, []skills.SkillSource) (*skills.SkillMeta, *skills.SkillBundle, skills.SkillSource) {
		return meta, bundle, nil
	}
	t.Cleanup(func() { hubResolveSourceMetaAndBundle = real })
}

func TestSkillsHubSourcesEndpointSourcesListsConfiguredHubs(t *testing.T) {
	c := newSessionClient(t)
	found := []skills.SkillMeta{fakeHubMeta("hermes-index/featured-skill", "trusted", "github")}
	fakeSourceRouter(t, &fakeHubSource{id: "official", found: found}, &fakeHubSource{id: "hermes-index", available: true, found: found},
		&fakeHubSource{id: "github", found: found})
	resp := c.get("/api/skills/hub/sources")
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	body := jsonObject(t, resp)
	ids := map[string]bool{}
	for _, s := range body["sources"].([]any) {
		row := s.(map[string]any)
		ids[row["id"].(string)] = true
		if label, _ := row["label"].(string); label == "" {
			t.Fatalf("source without a label: %v", row)
		}
	}
	if !ids["official"] || !ids["github"] || !ids["hermes-index"] || body["index_available"] != true {
		t.Fatalf("body %v", body)
	}
	featured := body["featured"].([]any)
	if len(featured) != 1 || featured[0].(map[string]any)["trust_level"] != "trusted" {
		t.Fatalf("featured %v", featured)
	}
	if _, ok := body["installed"].(map[string]any); !ok {
		t.Fatalf("installed %v", body["installed"])
	}
}

func TestOfficialSkillsCatalogEndpointListsFullCatalogWithInstalledFlags(t *testing.T) {
	c := newSessionClient(t)
	realLocal, realInstalled := hubListLocalOfficial, hubInstalledIdentifiers
	t.Cleanup(func() { hubListLocalOfficial, hubInstalledIdentifiers = realLocal, realInstalled })
	hubListLocalOfficial = func(context.Context) ([]skills.SkillMeta, error) {
		return []skills.SkillMeta{fakeHubMeta("official/gifs/gif-search", "builtin", "official"),
			fakeHubMeta("official/creative/ascii-art", "builtin", "official")}, nil
	}
	hubInstalledIdentifiers = func(context.Context, string) (*pyjson.Object, error) {
		return pyjson.ObjectOf("official/gifs/gif-search", pyjson.ObjectOf("name", "gif-search")), nil
	}
	resp := c.get("/api/skills/hub/official")
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	byIdent := map[string]map[string]any{}
	for _, s := range jsonObject(t, resp)["skills"].([]any) {
		row := s.(map[string]any)
		byIdent[row["identifier"].(string)] = row
	}
	gif, art := byIdent["official/gifs/gif-search"], byIdent["official/creative/ascii-art"]
	if len(byIdent) != 2 || gif == nil || art == nil {
		t.Fatalf("catalog %v", byIdent)
	}
	if gif["category"] != "gifs" || art["category"] != "creative" || gif["installed"] != true || art["installed"] != false {
		t.Fatalf("gif %v art %v", gif, art)
	}
}

func TestSkillsHubPreviewEndpointPreviewReturnsSkillMdText(t *testing.T) {
	c := newSessionClient(t)
	fakeSourceRouter(t)
	meta := fakeHubMeta("github/owner/repo/x", "community", "github")
	fakeHubResolve(t, &meta, fakeHubBundle("github/owner/repo/x"))
	resp := c.get("/api/skills/hub/preview?identifier=github/owner/repo/x")
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	body := jsonObject(t, resp)
	if md, _ := body["skill_md"].(string); !strings.Contains(md, "body text") {
		t.Fatalf("skill_md %q", md)
	}
	if !reflect.DeepEqual(body["files"], []any{"SKILL.md", "icon.png", "notes.txt"}) {
		t.Fatalf("files %v", body["files"])
	}
}

func TestSkillsHubPreviewEndpointPreview404WhenUnresolved(t *testing.T) {
	c := newSessionClient(t)
	fakeSourceRouter(t)
	fakeHubResolve(t, nil, nil)
	if resp := c.get("/api/skills/hub/preview?identifier=nope/x"); resp.Status != 404 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}

func TestSkillsHubScanEndpointScanReturnsVerdictAndPolicy(t *testing.T) {
	c := newSessionClient(t)
	fakeSourceRouter(t)
	fakeHubResolve(t, nil, fakeHubBundle("github/owner/repo/x"))
	realQuarantine, realScan, realRemove := hubQuarantineBundle, hubScanSkill, hubRemoveScanDir
	t.Cleanup(func() { hubQuarantineBundle, hubScanSkill, hubRemoveScanDir = realQuarantine, realScan, realRemove })
	fakePath := filepath.Join(t.TempDir(), "_fake_q")
	hubQuarantineBundle = func(context.Context, *skills.SkillBundle) (string, error) { return fakePath, nil }
	hubScanSkill = func(string, string) skills.ScanResult {
		return skills.ScanResult{SkillName: "x", Source: "github/owner/repo/x", TrustLevel: "community", Verdict: "caution", Summary: "s",
			Findings: []skills.Finding{{PatternID: "p", Severity: "high", Category: "exfiltration", File: "SKILL.md", Line: 10, Match: "m",
				Description: "leaks data"}}}
	}
	hubRemoveScanDir = func(string) error { return nil }
	resp := c.get("/api/skills/hub/scan?identifier=github/owner/repo/x")
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	body := jsonObject(t, resp)
	finding := body["findings"].([]any)[0].(map[string]any)
	if body["verdict"] != "caution" || body["trust_level"] != "community" || body["policy"] != "block" ||
		!pyjson.Equal(body["severity_counts"].(map[string]any)["high"], int64(1)) ||
		finding["category"] != "exfiltration" || finding["file"] != "SKILL.md" {
		t.Fatalf("body %s", fmt.Sprint(body))
	}
}

func TestProfileScopedHubActionsHubInstallScopedToDefaultStillCarriesTheSelector(t *testing.T) {
	c, defaultHome, workerHome := scopedProfiles(t)
	calls := spawnRecorder(t)
	t.Setenv("ICEBATH_HOME", workerHome)
	resp := c.send("POST", "/api/skills/hub/install", map[string]any{"identifier": "official/demo", "profile": "default"})
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	want := []string{"-p", "default", "skills", "install", "official/demo", "--yes"}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0].subcommand, want) {
		t.Fatalf("calls %v", *calls)
	}
	env, err := profileActionEnvironment(context.Background(), (*calls)[0].subcommand, nil)
	if err != nil {
		t.Fatal(err)
	}
	if env["ICEBATH_HOME"] != defaultHome {
		t.Fatalf("child home %q, want %q", env["ICEBATH_HOME"], defaultHome)
	}
}

func TestEditorEndpointsAuthEndpoints401WithoutToken(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		body         string
	}{
		{"GET", "/api/skills/content?name=dashboard-skill", ""},
		{"POST", "/api/skills", `{"name":"x","content":"y"}`},
		{"PUT", "/api/skills/content", `{"name":"x","content":"y"}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			c, _, _ := editorProfiles(t)
			var resp *testResponse
			if tc.body == "" {
				resp = c.request(tc.method, tc.path, nil)
			} else {
				resp = c.request(tc.method, tc.path, []byte(tc.body), "content-type", "application/json")
			}
			if resp.Status != 401 {
				t.Fatalf("status %d %s", resp.Status, resp.Body)
			}
		})
	}
}

func TestCronJobSkillsCreateJobWithSkills(t *testing.T) {
	c, _, _ := editorProfiles(t)
	resp := c.send("POST", "/api/cron/jobs", map[string]any{"prompt": "do work", "schedule": "every 1h", "name": "skilled-job",
		"skills": []any{"dashboard-skill"}})
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	job := jsonObject(t, resp)
	if !reflect.DeepEqual(job["skills"], []any{"dashboard-skill"}) {
		t.Fatalf("skills %v", job["skills"])
	}
	listed, _ := pyjson.Plain(c.get("/api/cron/jobs?profile=default").JSON(t)).([]any)
	matched := false
	for _, j := range listed {
		if row := j.(map[string]any); row["id"] == job["id"] {
			matched = reflect.DeepEqual(row["skills"], []any{"dashboard-skill"})
		}
	}
	if !matched {
		t.Fatalf("listing %v", listed)
	}
}

func TestCronJobSkillsUpdateJobSkills(t *testing.T) {
	c, _, _ := editorProfiles(t)
	job := jsonObject(t, c.send("POST", "/api/cron/jobs", map[string]any{"prompt": "do work", "schedule": "every 1h"}))
	if skillsField, held := job["skills"]; held && skillsField != nil && !reflect.DeepEqual(skillsField, []any{}) {
		t.Fatalf("new job skills %v", skillsField)
	}
	target := "/api/cron/jobs/" + job["id"].(string) + "?profile=default"
	resp := c.send("PUT", target, map[string]any{"updates": map[string]any{"skills": []any{"dashboard-skill", "worker-skill"}}})
	if resp.Status != 200 || !reflect.DeepEqual(jsonObject(t, resp)["skills"], []any{"dashboard-skill", "worker-skill"}) {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	resp = c.send("PUT", target, map[string]any{"updates": map[string]any{"skills": []any{}}})
	if resp.Status != 200 || !reflect.DeepEqual(jsonObject(t, resp)["skills"], []any{}) {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}
