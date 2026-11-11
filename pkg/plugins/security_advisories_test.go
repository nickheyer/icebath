package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

func advisoryFake() Advisory {
	return Advisory{
		ID:          "test-advisory-2026-99",
		Title:       "Test advisory",
		Summary:     "Pretend this package has been compromised.",
		URL:         "https://example.com/advisory",
		Compromised: []AdvisoryPackage{{Name: "fake-malicious-pkg", Versions: map[string]bool{"6.6.6": true}}},
		Remediation: []string{"pip uninstall -y fake-malicious-pkg", "Rotate any credentials that may have been exposed."},
		Published:   "2026-01-01",
		Severity:    "critical",
	}
}

// Points the home at a fresh sandbox holding a cache directory
func advisoryIsolatedHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(filepath.Join(h, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := fmt.Sprintf("_config_version: %d\nsecurity: {}\n", config.LatestConfigVersion())
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	return h
}

// Installs the given distributions as dist-info directories on a fresh import path
func advisoryInstalled(t *testing.T, table map[string]string) {
	t.Helper()
	site := t.TempDir()
	for name, version := range table {
		info := filepath.Join(site, distNormalized(name)+"-"+version+".dist-info")
		if err := os.MkdirAll(info, 0o755); err != nil {
			t.Fatal(err)
		}
		meta := "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + version + "\n"
		if err := os.WriteFile(filepath.Join(info, "METADATA"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PYTHONPATH", site)
}

func advisoryFakeHit() AdvisoryHit {
	return AdvisoryHit{Advisory: advisoryFake(), Package: "fake-malicious-pkg", InstalledVersion: "6.6.6"}
}

func TestDetectCompromisedNoMatchReturnsEmptyList(t *testing.T) {
	advisoryInstalled(t, map[string]string{})
	if hits := DetectCompromised([]Advisory{advisoryFake()}); len(hits) != 0 {
		t.Fatalf("hits = %#v", hits)
	}
}

func TestDetectCompromisedExactVersionMatch(t *testing.T) {
	advisoryInstalled(t, map[string]string{"fake-malicious-pkg": "6.6.6"})
	hits := DetectCompromised([]Advisory{advisoryFake()})
	if len(hits) != 1 {
		t.Fatalf("hits = %#v", hits)
	}
	if hits[0].Advisory.ID != advisoryFake().ID || hits[0].Package != "fake-malicious-pkg" || hits[0].InstalledVersion != "6.6.6" {
		t.Fatalf("hit = %#v", hits[0])
	}
}

func TestDetectCompromisedEmptyCompromisedSetMatchesAnyVersion(t *testing.T) {
	wildcard := Advisory{
		ID: "wildcard", Title: "Whole namespace owned", Summary: "x", URL: "x",
		Compromised: []AdvisoryPackage{{Name: "evil-namespace", Versions: map[string]bool{}}},
		Remediation: []string{"uninstall it"},
	}
	advisoryInstalled(t, map[string]string{"evil-namespace": "0.0.1"})
	hits := DetectCompromised([]Advisory{wildcard})
	if len(hits) != 1 || hits[0].InstalledVersion != "0.0.1" {
		t.Fatalf("hits = %#v", hits)
	}
}

func TestAckGetAckedIdsEmptyWhenNoConfig(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "home-is-a-file")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", blocked)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	if _, err := config.LoadContext(context.Background(), blocked, nil); err == nil {
		t.Fatal("the config of an unusable home loaded")
	}
	if got := GetAckedAdvisoryIDs(context.Background()); len(got) != 0 {
		t.Fatalf("acked = %v", got)
	}
}

func TestAckAckAdvisoryPersistsId(t *testing.T) {
	h := advisoryIsolatedHome(t)
	ctx := context.Background()
	count := func() int {
		config.ResetCache()
		cfg, err := config.LoadContext(ctx, h, nil)
		if err != nil {
			t.Fatal(err)
		}
		sec, _ := cfg["security"].(map[string]any)
		acked, _ := sec["acked_advisories"].([]any)
		n := 0
		for _, x := range acked {
			if x == "test-advisory-2026-99" {
				n++
			}
		}
		return n
	}
	if !AckAdvisory(ctx, "test-advisory-2026-99") {
		t.Fatal("ack was not stored")
	}
	if count() != 1 {
		t.Fatalf("stored %d times", count())
	}
	AckAdvisory(ctx, "test-advisory-2026-99")
	if count() != 1 {
		t.Fatalf("stored %d times after the second ack", count())
	}
	if !GetAckedAdvisoryIDs(ctx)["test-advisory-2026-99"] {
		t.Fatal("the stored ack is not read back")
	}
}

func TestBannerCacheFirstCallReturnsDueHits(t *testing.T) {
	advisoryIsolatedHome(t)
	hit := advisoryFakeHit()
	due := AdvisoryHitsDueForBanner(context.Background(), []AdvisoryHit{hit}, advisoryBannerRepeatHours)
	if !reflect.DeepEqual(due, []AdvisoryHit{hit}) {
		t.Fatalf("due = %#v", due)
	}
}

func TestBannerCacheCallAfterWindowReBanners(t *testing.T) {
	advisoryIsolatedHome(t)
	ctx := context.Background()
	hit := advisoryFakeHit()
	AdvisoryHitsDueForBanner(ctx, []AdvisoryHit{hit}, advisoryBannerRepeatHours)
	if again := AdvisoryHitsDueForBanner(ctx, []AdvisoryHit{hit}, advisoryBannerRepeatHours); len(again) != 0 {
		t.Fatalf("a banner shown a moment ago is due again: %#v", again)
	}
	cachePath, ok := advisoryBannerCachePath(ctx)
	if !ok {
		t.Fatal("no banner cache path")
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	var backdated []string
	for _, line := range pystr.Splitlines(string(data), false) {
		if parts := pystr.SplitWhitespace(line, 1); len(parts) == 2 {
			backdated = append(backdated, fmt.Sprintf("%s %f", parts[0], float64(time.Now().Unix()-48*3600)))
		}
	}
	if len(backdated) != 1 {
		t.Fatalf("cache lines = %q", string(data))
	}
	if err := os.WriteFile(cachePath, []byte(strings.Join(backdated, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	again := AdvisoryHitsDueForBanner(ctx, []AdvisoryHit{hit}, advisoryBannerRepeatHours)
	if !reflect.DeepEqual(again, []AdvisoryHit{hit}) {
		t.Fatalf("again = %#v", again)
	}
}

func TestBannerCacheAckedHitsNeverBanner(t *testing.T) {
	advisoryIsolatedHome(t)
	ctx := context.Background()
	hit := advisoryFakeHit()
	if !AckAdvisory(ctx, hit.Advisory.ID) {
		t.Fatal("ack was not stored")
	}
	config.ResetCache()
	if due := AdvisoryHitsDueForBanner(ctx, []AdvisoryHit{hit}, advisoryBannerRepeatHours); len(due) != 0 {
		t.Fatalf("due = %#v", due)
	}
	if got := AdvisoryStartupBanner(ctx, []AdvisoryHit{hit}); got != "" {
		t.Fatalf("banner = %q", got)
	}
	if got := AdvisoryGatewayLogMessage(ctx, []AdvisoryHit{hit}); got != "" {
		t.Fatalf("log message = %q", got)
	}
}

func TestRenderingFullRemediationTextContainsAllSteps(t *testing.T) {
	advisoryIsolatedHome(t)
	fake := advisoryFake()
	lines, err := AdvisoryFullRemediationText(context.Background(), advisoryFakeHit())
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n")
	for _, step := range fake.Remediation {
		if !strings.Contains(body, step) {
			t.Fatalf("step %q missing from %q", step, body)
		}
	}
	if !strings.Contains(body, fake.URL) || !strings.Contains(body, fake.Summary) {
		t.Fatalf("body = %q", body)
	}
}

func TestRealCatalogAdvisoriesWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, advisory := range Advisories {
		if advisory.ID == "" {
			t.Fatal("advisory has empty id")
		}
		if seen[advisory.ID] {
			t.Fatalf("duplicate id %s", advisory.ID)
		}
		seen[advisory.ID] = true
		if advisory.Title == "" || advisory.Summary == "" || len(advisory.Remediation) == 0 {
			t.Fatalf("%s: empty title, summary or remediation", advisory.ID)
		}
		if !strings.HasPrefix(advisory.URL, "http") {
			t.Fatalf("%s: bad url %q", advisory.ID, advisory.URL)
		}
		if len(advisory.Compromised) == 0 {
			t.Fatalf("%s: empty compromised tuple", advisory.ID)
		}
		for _, pkg := range advisory.Compromised {
			if pkg.Name == "" {
				t.Fatalf("%s: empty package name", advisory.ID)
			}
			if pkg.Versions == nil {
				t.Fatalf("%s: versions must be a set", advisory.ID)
			}
		}
		hit := AdvisoryHit{Advisory: advisory, Package: advisory.Compromised[0].Name, InstalledVersion: "0"}
		if _, err := AdvisoryFullRemediationText(context.Background(), hit); err != nil {
			t.Fatalf("%s: remediation does not render: %v", advisory.ID, err)
		}
	}
}

func TestAdvisoryRemediationUsesActiveHermesHome(t *testing.T) {
	tmp := t.TempDir()
	work := filepath.Join(tmp, ".icebath", "profiles", "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", work)
	hit := AdvisoryHit{Advisory: Advisories[0], Package: "mistralai", InstalledVersion: "2.4.6"}
	lines, err := AdvisoryFullRemediationText(context.Background(), hit)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "~/.icebath/profiles/work/.env") || strings.Contains(text, "{icebath_home}") || strings.Contains(text, "~/.icebath/.env") {
		t.Fatalf("text = %q", text)
	}
}
