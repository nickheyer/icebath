package profiles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
)

func TestCatalogDescribesVendorSpellings(t *testing.T) {
	if CatalogUpdated() == "" || len(Catalog()) < 20 {
		t.Fatal("embedded catalog must load with its date")
	}
	for id, want := range map[string]string{
		"claude-opus-5": "claude-opus-5",
		"anthropic.claude-sonnet-4-6-20260101-v1:0":    "claude-sonnet-4-6",
		"us.anthropic.claude-haiku-4-5-20251001-v1:0":  "claude-haiku-4-5",
		"anthropic/claude-sonnet-4.6":                  "claude-sonnet-4-6",
		"claude-opus-4-6@20260101":                     "claude-opus-4-6",
		"models/gemini-2.5-pro":                        "gemini-2.5-pro",
		"openai/gpt-5-mini":                            "gpt-5-mini",
		"gpt-5-2026-01-01":                             "gpt-5",
		"claude-haiku-4-5-20251001":                    "claude-haiku-4-5",
		"mistralai/mistral-large-latest":               "mistral-large-latest",
		"CLAUDE-SONNET-5":                              "claude-sonnet-5",
		"global.anthropic.claude-opus-4-7-20260301-v1": "claude-opus-4-7",
	} {
		m, ok := Describe(id)
		if !ok || m.ID != want {
			t.Fatalf("%s described as %q (%v), want %s", id, m.ID, ok, want)
		}
	}
	if _, ok := Describe("not-a-model"); ok {
		t.Fatal("unknown model described")
	}
	if _, ok := Describe(""); ok {
		t.Fatal("empty id described")
	}
	m, _ := Describe("claude-opus-5")
	if m.Context != 1000000 || m.MaxOutput != 128000 || m.Pricing == nil || m.Pricing.Input != 5 || m.Pricing.Output != 25 || m.Capabilities == nil || !m.Capabilities.Thinking {
		t.Fatalf("%+v", m)
	}
}

func TestPricingCost(t *testing.T) {
	p := Pricing{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25}
	u := protocol.Usage{Input: 1_000_000, Output: 100_000, CacheRead: 2_000_000, CacheWrite: 400_000, Reasoning: 50_000}
	want := 5 + 2.5 + 1 + 2.5
	if got := p.Cost(u); got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("cost %v want %v", got, want)
	}
	if (Pricing{}).Known() || !p.Known() {
		t.Fatal("known")
	}
	if (Model{}).Cost(u) != 0 || (Model{Pricing: &p}).Cost(u) != p.Cost(u) {
		t.Fatal("model cost")
	}
}

func TestEnrichAndLoadCatalog(t *testing.T) {
	listed := []Model{{ID: "claude-sonnet-4-6-20260101", Created: 7}, {ID: "mystery-9000", Context: 10}}
	out := Enrich(listed)
	if out[0].Name != "Claude Sonnet 4.6" || out[0].Context != 1000000 || out[0].Pricing == nil || out[0].Created != 7 {
		t.Fatalf("%+v", out[0])
	}
	if out[1].Name != "" || out[1].Pricing != nil || out[1].Context != 10 {
		t.Fatalf("unknown models must pass through untouched: %+v", out[1])
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")
	os.WriteFile(path, []byte(`{"models":[{"id":"mystery-9000","name":"Mystery","context":4096,"pricing":{"input":1,"output":2}}]}`), 0o644)
	added, err := LoadCatalogFile(path)
	if err != nil || len(added) != 1 {
		t.Fatal(err)
	}
	if m, ok := Describe("mystery-9000"); !ok || m.Name != "Mystery" || m.Pricing.Output != 2 {
		t.Fatalf("%+v %v", m, ok)
	}
	if _, err := LoadCatalog(strings.NewReader(`[{"id":"claude-opus-5","name":"Overridden"}]`)); err != nil {
		t.Fatal(err)
	}
	if m, _ := Describe("claude-opus-5"); m.Name != "Overridden" {
		t.Fatalf("added entries must win: %+v", m)
	}
	if _, err := LoadCatalog(strings.NewReader(`[{"name":"no id"}]`)); err == nil {
		t.Fatal("entry without id accepted")
	}
	if _, err := LoadCatalog(strings.NewReader(`{bad`)); err == nil {
		t.Fatal("bad json accepted")
	}
	if _, err := LoadCatalogFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestConfigDirAndUserProfiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ConfigDirEnv, dir)
	got, err := ConfigDir()
	if err != nil || got != dir {
		t.Fatalf("%v %q", err, got)
	}
	list, err := LoadUser()
	if err != nil || list != nil {
		t.Fatalf("missing user file must yield nothing: %v %v", err, list)
	}
	path, _ := UserPath()
	os.WriteFile(path, []byte(`[{"name":"mine","protocol":"openai-completions","base_url":"http://localhost:1/v1"}]`), 0o644)
	list, err = LoadUser()
	if err != nil || len(list) != 1 || list[0].Name != "mine" {
		t.Fatalf("%v %+v", err, list)
	}
	os.WriteFile(path, []byte(`{"live":"mine","profiles":{"mine":{"API_KEY":"k","BASE_URL":"http://localhost:1","MODEL":"m"}}}`), 0o644)
	doc, err := ReadUser()
	if err != nil || doc.Default != "mine" || len(doc.Profiles) != 1 || doc.Profiles[0].BaseURL != "http://localhost:1/v1" || doc.Profiles[0].Model() != "m" {
		t.Fatalf("the earlier user file shape must load: %v %+v", err, doc)
	}
	os.WriteFile(path, []byte(`{`), 0o644)
	if _, err := ReadUser(); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("a broken user file must be named: %v", err)
	}
	os.Remove(path)
	if doc, err := ReadUser(); err != nil || doc.Default != "" || doc.Profiles != nil {
		t.Fatalf("missing user file must yield an empty document: %v %+v", err, doc)
	}
	t.Setenv(ConfigDirEnv, "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	got, err = ConfigDir()
	if err != nil || !strings.HasSuffix(got, filepath.Join("xdg", "icebath")) {
		t.Fatalf("%v %q", err, got)
	}
}

func TestProfileCloneAndBaseURLOverride(t *testing.T) {
	Register(Profile{Name: "clonable", Protocol: "openai-completions", BaseURL: "http://a/v1", Models: []string{"m"}, Options: map[string]string{"k": "v"}, Extra: map[string]json.RawMessage{"x": json.RawMessage(`1`)}, BaseURLEnv: "CLONABLE_BASE_URL"})
	p, _ := Lookup("clonable")
	p.Models[0] = "changed"
	p.Options["k"] = "changed"
	p.Extra["x"] = json.RawMessage(`2`)
	again, _ := Lookup("clonable")
	if again.Models[0] != "m" || again.Options["k"] != "v" || string(again.Extra["x"]) != "1" {
		t.Fatalf("lookup must not alias the registry: %+v", again)
	}
	for _, d := range Defaults() {
		if d.Name == "clonable" {
			d.Models[0] = "changed"
		}
	}
	if again, _ = Lookup("clonable"); again.Models[0] != "m" {
		t.Fatal("defaults must not alias the registry")
	}
	if again.Resolved().BaseURL != "http://a/v1" {
		t.Fatal("unset override must keep the base url")
	}
	t.Setenv("CLONABLE_BASE_URL", "http://b/v2/")
	if again.Resolved().BaseURL != "http://b/v2" {
		t.Fatalf("%q", again.Resolved().BaseURL)
	}
	if again.Model() != "m" {
		t.Fatal("default model")
	}
}
