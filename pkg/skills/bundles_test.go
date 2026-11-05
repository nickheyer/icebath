package skills

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Bundles dir override and skills dir scope with the bundle cache reset
func bundlesEnv(t *testing.T) (*Scope, string, string) {
	t.Helper()
	s, skills, _ := viewScope(t)
	bundles := filepath.Join(filepath.Dir(skills), "skill-bundles")
	t.Setenv("ICEBATH_BUNDLES_DIR", bundles)
	resetBundleCache(t)
	return s, bundles, skills
}

func resetBundleCache(t *testing.T) {
	bundleCache.Lock()
	bundleCache.bundles, bundleCache.sig, bundleCache.stamped = nil, nil, false
	bundleCache.Unlock()
	t.Cleanup(func() {
		bundleCache.Lock()
		bundleCache.bundles, bundleCache.sig, bundleCache.stamped = nil, nil, false
		bundleCache.Unlock()
	})
}

// Writes a bundle YAML the way the Python test helper lays it out
func makeBundleYAML(t *testing.T, dir, slug string, skills []string, description, instruction string) string {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	lines := []string{"name: " + slug}
	if description != "" {
		lines = append(lines, "description: "+description)
	}
	lines = append(lines, "skills:")
	for _, s := range skills {
		lines = append(lines, "  - "+s)
	}
	if instruction != "" {
		lines = append(lines, "instruction: |")
		for _, ln := range strings.Split(instruction, "\n") {
			lines = append(lines, "  "+ln)
		}
	}
	p := filepath.Join(dir, slug+".yaml")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustBundles(t *testing.T, s *Scope) SkillCommands {
	t.Helper()
	b, err := s.GetSkillBundles()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBundleSlugify(t *testing.T) {
	if SlugifySkillName("Backend Dev") != "backend-dev" || SlugifySkillName("") != "" || SlugifySkillName("!!!") != "" {
		t.Fatal("slugify")
	}
}

func TestScanBundlesFindsAndSkipsInvalid(t *testing.T) {
	s, dir, _ := bundlesEnv(t)
	makeBundleYAML(t, dir, "backend", []string{"skill-a", "skill-b"}, "", "")
	os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("{not: valid yaml: ["), 0o644)
	out, err := s.ScanBundles()
	if err != nil {
		t.Fatal(err)
	}
	if _, bad := out["/broken"]; bad || out["/backend"]["name"] != "backend" || !reflect.DeepEqual(out["/backend"]["skills"], []any{"skill-a", "skill-b"}) {
		t.Fatalf("scan %v", out)
	}
	if info := out["/backend"]; info["description"] != "Load 2 skills as a bundle" || info["instruction"] != "" || info["slug"] != "backend" {
		t.Fatalf("defaults %v", info)
	}
}

func TestBundleFileShapesAreSkippedOrRaised(t *testing.T) {
	s, dir, _ := bundlesEnv(t)
	os.MkdirAll(dir, 0o755)
	for name, body := range map[string]string{"list.yaml": "- a\n", "noskills.yaml": "name: x\n", "scalar.yaml": "name: y\nskills: a\n",
		"blank.yaml": "name: z\nskills: ['  ', '']\n", "punct.yaml": "name: '!!!'\nskills: [a]\n"} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "stem.yml"), []byte("\xef\xbb\xbfskills: [1, true, ~]\ndescription: 0\n"), 0o644)
	out, err := s.ScanBundles()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sortedNames(out), []string{"/stem"}) || !reflect.DeepEqual(out["/stem"]["skills"], []any{"1", "True", "None"}) || out["/stem"]["description"] != "Load 3 skills as a bundle" {
		t.Fatalf("shapes %v", out)
	}
	os.WriteFile(filepath.Join(dir, "zz.yaml"), []byte("name: \xff\n"), 0o644)
	if _, err := s.ScanBundles(); err == nil || !strings.Contains(err.Error(), "'utf-8' codec can't decode byte 0xff in position 6: invalid start byte") {
		t.Fatalf("decode error %v", err)
	}
}

func TestGetSkillBundlesRescansOnChange(t *testing.T) {
	s, dir, _ := bundlesEnv(t)
	makeBundleYAML(t, dir, "a", []string{"s1"}, "", "")
	if _, ok := mustBundles(t, s)["/a"]; !ok {
		t.Fatal("first scan")
	}
	makeBundleYAML(t, dir, "b", []string{"s2"}, "", "")
	if got := sortedNames(mustBundles(t, s)); !reflect.DeepEqual(got, []string{"/a", "/b"}) {
		t.Fatalf("rescan %v", got)
	}
}

func TestResolveBundleCommandKey(t *testing.T) {
	s, dir, _ := bundlesEnv(t)
	makeBundleYAML(t, dir, "my-bundle", []string{"s1"}, "", "")
	if key, ok, err := s.ResolveBundleCommandKey("my-bundle"); err != nil || !ok || key != "/my-bundle" {
		t.Fatalf("exact %q %v %v", key, ok, err)
	}
	if key, ok, _ := s.ResolveBundleCommandKey("my_bundle"); !ok || key != "/my-bundle" {
		t.Fatalf("underscore %q %v", key, ok)
	}
	for _, c := range []string{"missing", ""} {
		if _, ok, _ := s.ResolveBundleCommandKey(c); ok {
			t.Fatalf("%q resolved", c)
		}
	}
}

func TestBuildBundleInvocationMessage(t *testing.T) {
	ctx := context.Background()
	s, dir, skills := bundlesEnv(t)
	makeCmdSkill(t, skills, "skill-a", "", "Skill A content.", "")
	makeCmdSkill(t, skills, "skill-b", "", "Skill B content.", "")
	makeBundleYAML(t, dir, "combo", []string{"skill-a", "skill-b"}, "", "")
	msg, loaded, missing, ok, err := s.BuildBundleInvocationMessage(ctx, "/combo", "", "", "")
	sort.Strings(loaded)
	if err != nil || !ok || !reflect.DeepEqual(loaded, []string{"skill-a", "skill-b"}) || len(missing) != 0 ||
		!strings.Contains(msg, "Skill A content.") || !strings.Contains(msg, "Skill B content.") || !strings.Contains(msg, "combo") {
		t.Fatalf("all %v %v %v %v %q", ok, err, loaded, missing, msg)
	}
	makeBundleYAML(t, dir, "ghost", []string{"skill-a", "skill-ghost"}, "", "")
	msg, loaded, missing, ok, _ = s.BuildBundleInvocationMessage(ctx, "/ghost", "", "", "")
	if !ok || !reflect.DeepEqual(loaded, []string{"skill-a"}) || !reflect.DeepEqual(missing, []string{"skill-ghost"}) || !strings.Contains(msg, "skill-ghost") {
		t.Fatalf("missing %v %v %q", loaded, missing, msg)
	}
	if _, _, _, ok, _ := s.BuildBundleInvocationMessage(ctx, "/nope", "", "", ""); ok {
		t.Fatal("unknown bundle built")
	}
}

func TestBundleSkipsPlatformDisabledSkills(t *testing.T) {
	ctx := context.Background()
	s, dir, skills := bundlesEnv(t)
	makeCmdSkill(t, skills, "skill-a", "", "Skill A content.", "")
	makeCmdSkill(t, skills, "skill-b", "", "SECRET DISABLED CONTENT.", "")
	makeBundleYAML(t, dir, "combo", []string{"skill-a", "skill-b"}, "", "")
	setSkillsCfg(s, "platform_disabled", map[string]any{"telegram": []any{"skill-b"}})
	msg, loaded, _, ok, _ := s.BuildBundleInvocationMessage(ctx, "/combo", "", "", "telegram")
	if !ok || !reflect.DeepEqual(loaded, []string{"skill-a"}) || strings.Contains(msg, "SECRET DISABLED CONTENT.") || !strings.Contains(msg, "skill-b") || !strings.Contains(strings.ToLower(msg), "disabled") {
		t.Fatalf("telegram %v %q", loaded, msg)
	}
	msg, loaded, _, ok, _ = s.BuildBundleInvocationMessage(ctx, "/combo", "", "", "")
	sort.Strings(loaded)
	if !ok || !reflect.DeepEqual(loaded, []string{"skill-a", "skill-b"}) || !strings.Contains(msg, "SECRET DISABLED CONTENT.") {
		t.Fatalf("no platform %v", loaded)
	}
}

func TestBundleCarriesItsTypedKeys(t *testing.T) {
	s, _ := describeScope(t)
	dir := filepath.Join(t.TempDir(), "skill-bundles")
	t.Setenv("ICEBATH_BUNDLES_DIR", dir)
	resetBundleCache(t)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "demo.yaml"), []byte("name: demo\nskills:\n  - work\n  - clean\n"), 0o644)
	msg, _, _, ok, err := s.BuildBundleInvocationMessage(context.Background(), "/demo", "fix the title leak", "", "")
	if err != nil || !ok {
		t.Fatalf("build %v %v", ok, err)
	}
	if d := describe(msg); !strings.HasSuffix(d, "— fix the title leak") || strings.Contains(d, "worktree") {
		t.Fatalf("described %q", d)
	}
}

func TestSaveAndDeleteBundle(t *testing.T) {
	s, dir, _ := bundlesEnv(t)
	path, err := s.SaveBundle("test-bundle", []string{"s1", "s2"}, "d", "i", false)
	if err != nil || filepath.Dir(path) != dir {
		t.Fatalf("save %q %v", path, err)
	}
	want := "name: test-bundle\nskills:\n  - s1\n  - s2\ndescription: d\ninstruction: i\n"
	if runtime.GOOS == "windows" {
		want = strings.ReplaceAll(want, "\n", "\r\n")
	}
	if raw, _ := os.ReadFile(path); string(raw) != want {
		t.Fatalf("content %q", raw)
	}
	if _, err := s.SaveBundle("test-bundle", []string{"s3"}, "", "", false); err == nil || err.(*BundleError).Class != "FileExistsError" {
		t.Fatalf("exists %v", err)
	}
	s.SaveBundle("dup", []string{"s1"}, "", "", false)
	os.Chmod(filepath.Join(dir, "dup.yaml"), 0o600)
	s.SaveBundle("dup", []string{"s2"}, "", "", true)
	if info, ok, _ := s.GetBundle("dup"); !ok || !reflect.DeepEqual(info["skills"], []any{"s2"}) {
		t.Fatalf("overwrite %v", info)
	}
	if st, _ := os.Stat(filepath.Join(dir, "dup.yaml")); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode not preserved %v", st.Mode())
	}
	s.SaveBundle("doomed", []string{"s1"}, "", "", false)
	if _, ok, _ := s.GetBundle("doomed"); !ok {
		t.Fatal("doomed missing")
	}
	if _, err := s.DeleteBundle("doomed"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetBundle("doomed"); ok {
		t.Fatal("doomed kept")
	}
	if _, err := s.DeleteBundle("doomed"); err == nil || err.(*BundleError).Class != "FileNotFoundError" {
		t.Fatalf("delete missing %v", err)
	}
	for _, tc := range []struct {
		name   string
		skills []string
		msg    string
	}{{" ", []string{"a"}, "Bundle name is required"}, {"x", []string{" "}, "Bundle must reference at least one skill"}, {"!!!", []string{"a"}, "Bundle name '!!!' normalizes to an empty slug"}} {
		if _, err := s.SaveBundle(tc.name, tc.skills, "", "", false); err == nil || err.(*BundleError).Class != "ValueError" || err.Error() != tc.msg {
			t.Fatalf("%q %v", tc.name, err)
		}
	}
}

func TestReloadBundlesReportsAddedAndRemoved(t *testing.T) {
	s, dir, _ := bundlesEnv(t)
	makeBundleYAML(t, dir, "old", []string{"s1"}, "", "")
	s.ScanBundles()
	os.Remove(filepath.Join(dir, "old.yaml"))
	makeBundleYAML(t, dir, "new", []string{"s2"}, "", "")
	diff, err := s.ReloadBundles()
	if err != nil {
		t.Fatal(err)
	}
	names := func(key string) []string {
		var out []string
		for _, e := range diff[key].([]map[string]any) {
			out = append(out, e["name"].(string))
		}
		return out
	}
	if !reflect.DeepEqual(names("added"), []string{"new"}) || !reflect.DeepEqual(names("removed"), []string{"old"}) || diff["total"] != 1 {
		t.Fatalf("diff %v", diff)
	}
}

func TestListBundlesSortedBySlug(t *testing.T) {
	s, dir, _ := bundlesEnv(t)
	for _, n := range []string{"zebra", "apple", "mango"} {
		makeBundleYAML(t, dir, n, []string{"s1"}, "", "")
	}
	list, _ := s.ListBundles()
	var slugs []string
	for _, b := range list {
		slugs = append(slugs, b["slug"].(string))
	}
	if !reflect.DeepEqual(slugs, []string{"apple", "mango", "zebra"}) {
		t.Fatalf("slugs %v", slugs)
	}
}

var pinnedTick = time.Unix(0, 1_800_000_000_000_000_000)

func pin(paths ...string) {
	for _, p := range paths {
		os.Chtimes(p, pinnedTick, pinnedTick)
	}
}

func TestBundleCacheSeesSameTickChanges(t *testing.T) {
	s, dir, _ := bundlesEnv(t)
	alpha := makeBundleYAML(t, dir, "alpha", []string{"x"}, "", "")
	pin(alpha, dir)
	if got := sortedNames(mustBundles(t, s)); !reflect.DeepEqual(got, []string{"/alpha"}) {
		t.Fatalf("first %v", got)
	}
	beta := makeBundleYAML(t, dir, "beta", []string{"y"}, "", "")
	pin(beta, dir)
	if got := sortedNames(mustBundles(t, s)); !reflect.DeepEqual(got, []string{"/alpha", "/beta"}) {
		t.Fatalf("new bundle %v", got)
	}
	makeBundleYAML(t, dir, "alpha", []string{"changed"}, "", "")
	pin(alpha)
	if got := mustBundles(t, s)["/alpha"]["skills"]; !reflect.DeepEqual(got, []any{"changed"}) {
		t.Fatalf("edit %v", got)
	}
	raw, _ := os.ReadFile(alpha)
	os.WriteFile(alpha, []byte(strings.Replace(string(raw), "changed", "cHANGED", 1)), 0o644)
	pin(alpha)
	if got := mustBundles(t, s)["/alpha"]["skills"]; !reflect.DeepEqual(got, []any{"cHANGED"}) {
		t.Fatalf("same size rewrite %v", got)
	}
}

func TestWriteLandingDuringScanIsPickedUpNextCall(t *testing.T) {
	s, dir, _ := bundlesEnv(t)
	alpha := makeBundleYAML(t, dir, "alpha", []string{"x"}, "", "")
	pin(alpha, dir)
	real := bundleSteps.load
	t.Cleanup(func() { bundleSteps.load = real })
	bundleSteps.load = func(sc *Scope, path string) (map[string]any, error) {
		info, err := real(sc, path)
		makeBundleYAML(t, dir, "alpha", []string{"after-parse"}, "", "")
		pin(alpha)
		return info, err
	}
	if got := mustBundles(t, s)["/alpha"]["skills"]; !reflect.DeepEqual(got, []any{"x"}) {
		t.Fatalf("during %v", got)
	}
	bundleSteps.load = real
	if got := mustBundles(t, s)["/alpha"]["skills"]; !reflect.DeepEqual(got, []any{"after-parse"}) {
		t.Fatalf("after %v", got)
	}
}

func TestBundleOverwriteNeverExposesAPartialFile(t *testing.T) {
	s, _, _ := bundlesEnv(t)
	if _, err := s.SaveBundle("alpha", []string{"old-skill"}, "", "", false); err != nil {
		t.Fatal(err)
	}
	target, _ := s.BundlePathFor("alpha")
	var mu sync.Mutex
	var seen []any
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(target)
			if err != nil {
				continue
			}
			text, _ := decodeUTF8Sig(raw)
			doc, _ := yamlLoad(text)
			m, _ := doc.(map[string]any)
			mu.Lock()
			seen = append(seen, m["skills"])
			mu.Unlock()
		}
	}()
	for i := 0; i < 20; i++ {
		skills := []string{"new-skill"}
		if i%2 == 1 {
			skills = []string{"old-skill"}
		}
		if _, err := s.SaveBundle("alpha", skills, "", "", true); err != nil {
			t.Fatal(err)
		}
	}
	s.SaveBundle("alpha", []string{"new-skill"}, "", "", true)
	close(stop)
	<-done
	for _, v := range seen {
		if !reflect.DeepEqual(v, []any{"old-skill"}) && !reflect.DeepEqual(v, []any{"new-skill"}) {
			t.Fatalf("partial bundle observed %v", v)
		}
	}
	if info, _, _ := s.GetBundle("alpha"); !reflect.DeepEqual(info["skills"], []any{"new-skill"}) || len(seen) == 0 {
		t.Fatalf("final %v seen %d", info, len(seen))
	}
}
