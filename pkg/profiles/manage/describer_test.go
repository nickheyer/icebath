package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/profiles"
)

// Registered profile myprof whose auxiliary reply is content
func describerProfile(t *testing.T, content string) string {
	t.Helper()
	tmp := manageEnv(t)
	dir := namedProfile(t, tmp, "myprof")
	real := describeCallLLM
	describeCallLLM = func(context.Context, string, []map[string]any, float64, int, time.Duration) (string, string, error) {
		return content, "", nil
	}
	t.Cleanup(func() { describeCallLLM = real })
	return dir
}

func describe(t *testing.T, overwrite bool) DescribeOutcome {
	t.Helper()
	out, err := DescribeProfile(context.Background(), "myprof", overwrite, 0)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDescriberWritesDescriptionWithAutoTrue(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"description": "writes Python codebases"})
	dir := describerProfile(t, string(payload))
	out := describe(t, false)
	if !out.OK || out.Description == nil || *out.Description != "writes Python codebases" {
		t.Fatalf("outcome = %+v", out)
	}
	meta := profiles.ReadProfileMeta(dir)
	if meta.Description != "writes Python codebases" || !meta.DescriptionAuto {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestDescriberRefusesJSONShapedReplyThatDoesNotParse(t *testing.T) {
	for _, raw := range []string{
		"{\n  \"description\": \"Generalist agent that writes and debugs code, orchestrates autonomous sub-agents, and automates macOS/App",
		"{\n  \"desc",
		"```json\n{\n  \"description\": \"Generalist agent that writes and debugs cod",
		"```JSON\n{\n  \"description\": \"Generalist agent that writes and debugs cod",
	} {
		dir := describerProfile(t, raw)
		prev, auto := "previous", true
		if err := profiles.WriteProfileMeta(dir, profiles.MetaUpdate{Description: &prev, DescriptionAuto: &auto}); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(filepath.Join(dir, "profile.yaml"))
		if out := describe(t, true); out.OK {
			t.Fatalf("accepted %q", raw)
		}
		if after, _ := os.ReadFile(filepath.Join(dir, "profile.yaml")); !bytes.Equal(before, after) {
			t.Fatalf("profile.yaml changed for %q", raw)
		}
	}
}

func TestDescriberStillAcceptsPlainProseFallback(t *testing.T) {
	dir := describerProfile(t, "Writes and debugs Python codebases.\n\nSecond paragraph is dropped.")
	out := describe(t, false)
	if !out.OK || *out.Description != "Writes and debugs Python codebases." {
		t.Fatalf("outcome = %+v", out)
	}
	if profiles.ReadProfileMeta(dir).Description != *out.Description {
		t.Fatal("description not stored")
	}
}

func TestDescriberRefusesToOverwriteUserAuthored(t *testing.T) {
	dir := describerProfile(t, `{"description": "new"}`)
	curated, auto := "curated", false
	if err := profiles.WriteProfileMeta(dir, profiles.MetaUpdate{Description: &curated, DescriptionAuto: &auto}); err != nil {
		t.Fatal(err)
	}
	if out := describe(t, false); out.OK {
		t.Fatalf("outcome = %+v", out)
	}
	if profiles.ReadProfileMeta(dir).Description != "curated" {
		t.Fatal("user description replaced")
	}
}

func TestDescriberSkillSamplingAndListing(t *testing.T) {
	tmp := manageEnv(t)
	dir := namedProfile(t, tmp, "myprof")
	writeFile(t, filepath.Join(dir, "skills", "dev", "tool", "SKILL.md"), "x")
	writeFile(t, filepath.Join(dir, "skills", "solo", "SKILL.md"), "x")
	writeFile(t, filepath.Join(dir, "skills", ".git", "x", "SKILL.md"), "x")
	if got := collectSkills(dir); len(got) != 2 || got[0] != "dev/tool" || got[1] != "solo" {
		t.Fatalf("skills = %v", got)
	}
	names := make([]string, 120)
	for i := range names {
		names[i] = string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	if got := sampleSkills(names); len(got) != MaxSkillsForPrompt || got[1] != names[2] {
		t.Fatalf("sample = %v", got)
	}
	curated, auto := "curated", false
	profiles.WriteProfileMeta(dir, profiles.MetaUpdate{Description: &curated, DescriptionAuto: &auto})
	namedProfile(t, tmp, "other")
	all, err := ListDescribableProfiles(context.Background(), false)
	if err != nil || !containsAll(all, "default", "myprof", "other") {
		t.Fatalf("all = %v, %v", all, err)
	}
	missing, _ := ListDescribableProfiles(context.Background(), true)
	if contains(missing, "myprof") || !contains(missing, "other") {
		t.Fatalf("missing = %v", missing)
	}
}
