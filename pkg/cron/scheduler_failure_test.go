package cron

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/tools"
)

func strp(s string) *string { return &s }

func noChainStore(t *testing.T) *Store {
	t.Helper()
	s, _ := newTestStore(t)
	s.loadConfig = func() (config.Tree, error) { return config.Tree{}, nil }
	return s
}

var failureNoticeJob = Job{Name: "Morning brief", ID: "ab12cd34"}

var httpLead = regexp.MustCompile(`failed: (HTTP|Error code:|provider )`)

func TestAuthFailureNamesThePinnedProviderAndTheFailingProfile(t *testing.T) {
	tmp := t.TempDir()
	profileHome := filepath.Join(tmp, ".icebath", "profiles", "ops")
	if err := os.MkdirAll(filepath.Join(profileHome, "cron"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", profileHome)
	s := openCronStore(profileHome, nil)
	s.loadConfig = func() (config.Tree, error) { return config.Tree{}, nil }
	pinned := failureNoticeJob
	pinned.Provider = strp("openai-codex")
	msg := s.summarizeCronFailureForDelivery(&pinned, strp("Error code: 401 - Unauthorized"))
	if !strings.Contains(msg, "`icebath -p ops auth add openai-codex --type oauth`") || strings.Contains(msg, "<provider>") {
		t.Fatal(msg)
	}
	job := failureNoticeJob
	unpinned := s.summarizeCronFailureForDelivery(&job, strp("Error code: 401 - Unauthorized"))
	if !strings.Contains(unpinned, "`icebath -p ops auth add <provider>`") {
		t.Fatal(unpinned)
	}
}

func TestRateAndUsageLimitPhrasesStillYieldAProviderNotice(t *testing.T) {
	s := noChainStore(t)
	for _, text := range []string{
		"Nous Portal rate limit active until 15:00",
		"RuntimeError: usage limit reached for this key",
		"You have hit your weekly usage limit",
		"insufficient quota",
	} {
		job := failureNoticeJob
		msg := s.summarizeCronFailureForDelivery(&job, strp(text))
		if !strings.Contains(strings.ToLower(msg), "limit") || httpLead.MatchString(msg) {
			t.Fatal(msg)
		}
		if !strings.Contains(msg, "`icebath cron run ab12cd34`") && !strings.Contains(msg, "`icebath cron edit ab12cd34") {
			t.Fatal(msg)
		}
	}
}

func TestCronCauseGlossIsTheSharedTable(t *testing.T) {
	s := noChainStore(t)
	for reason := range tools.FailureCauseGlossTable {
		notice, ok := providerFailureNotice(s.home, "Morning brief", "ab12cd34", reason, "x.", nil)
		if !ok || !strings.Contains(notice, "`icebath cron") {
			t.Fatal(reason)
		}
	}
	if _, ok := providerFailureNotice(s.home, "Morning brief", "ab12cd34", "unknown", "x.", nil); ok {
		t.Fatal("unknown reason must have no notice")
	}
}

func TestSummarizeCronFailureForDelivery(t *testing.T) {
	s := noChainStore(t)
	embedded := s.summarizeCronFailureForDelivery(&Job{Name: "LLM Wiki Incremental Index", NoAgent: true}, strp("Script failed: path/hash429abc.md source snapshot failure"))
	if strings.Contains(embedded, "rate-limited") || !strings.Contains(embedded, "hash429abc.md") {
		t.Fatal(embedded)
	}
	limited := s.summarizeCronFailureForDelivery(&Job{Name: "provider-backed job"}, strp("HTTP 429: Too Many Requests"))
	if !strings.Contains(limited, "rate-limited") || !strings.Contains(strings.ToLower(limited), "backup provider") {
		t.Fatal(limited)
	}
	script := s.summarizeCronFailureForDelivery(&Job{Name: "script job", NoAgent: true}, strp("HTTP 429: Too Many Requests"))
	if strings.Contains(strings.ToLower(script), "ai model service") || strings.Contains(strings.ToLower(script), "backup provider") {
		t.Fatal(script)
	}
	timeout := s.summarizeCronFailureForDelivery(&Job{Name: "script job", NoAgent: true}, strp("Script timed out after 3600s"))
	if !strings.Contains(timeout, "script timed out") || strings.Contains(timeout, "did not respond in time") || strings.Contains(strings.ToLower(timeout), "backup provider") {
		t.Fatal(timeout)
	}
	idle := s.summarizeCronFailureForDelivery(&Job{Name: "j", ID: "x"}, strp("Agent idle for 600s (limit 600s)"))
	if !strings.Contains(idle, "the job stalled") {
		t.Fatal(idle)
	}
	prev := detectGatewayCodeSkew
	detectGatewayCodeSkew = func() (string, string, bool) { return "aaa", "bbb", true }
	t.Cleanup(func() { detectGatewayCodeSkew = prev })
	skew := s.summarizeCronFailureForDelivery(&Job{Name: "j", ID: "x"}, strp("ImportError: cannot import name 'x' from 'y'"))
	if !strings.Contains(skew, "booted on aaa, disk is at bbb") {
		t.Fatal(skew)
	}
	long := s.summarizeCronFailureForDelivery(&Job{Name: "j", ID: "x", NoAgent: true}, strp("ValueError: "+strings.Repeat("word ", 80)))
	if !strings.Contains(long, "...") || strings.Contains(long, "ValueError") {
		t.Fatal(long)
	}
}

func TestPinnedFallbackPhrase(t *testing.T) {
	s := noChainStore(t)
	job := &Job{ID: "p1", Model: strp("gpt-x")}
	if got := s.fallbackChainPhrase(job); !strings.Contains(got, "`icebath cron edit p1 --unpin`") {
		t.Fatal(got)
	}
	if got := s.fallbackChainPhrase(&Job{ID: "u"}); !strings.Contains(got, "No backup provider is configured") {
		t.Fatal(got)
	}
	s.loadConfig = func() (config.Tree, error) { panic("broken") }
	if got := s.fallbackChainPhrase(&Job{ID: "u"}); got != "No backup provider succeeded either." {
		t.Fatal(got)
	}
	if !jobRoutePinned(&Job{BaseURL: strp(" http://x ")}) || jobRoutePinned(&Job{Provider: strp("  ")}) {
		t.Fatal("pin detection")
	}
	if len(jobFallbackChain(job, config.Tree{"fallback_providers": []any{map[string]any{"provider": "openrouter", "model": "m"}}})) != 0 {
		t.Fatal("pinned job borrowed the chain")
	}
}

type titleDB struct {
	calls   []string
	fail    int
	lineage []string
}

func (d *titleDB) SetSessionTitle(ctx context.Context, id, title string) (bool, error) {
	d.calls = append(d.calls, title)
	if d.fail > 0 {
		d.fail--
		return false, &pyint.Error{Kind: "ValueError", Msg: "in use"}
	}
	return true, nil
}

func (d *titleDB) GetNextTitleInLineage(ctx context.Context, base string) (string, error) {
	d.lineage = append(d.lineage, base)
	return base + " #2", nil
}

func TestSetCronSessionTitle(t *testing.T) {
	db := &titleDB{fail: 1}
	out, err := setCronSessionTitle(context.Background(), db, "sess-1", "Nightly Synthesis")
	if err != nil || out != "Nightly Synthesis #2" || len(db.lineage) != 1 || db.lineage[0] != "Nightly Synthesis" {
		t.Fatal(out, err, db.lineage)
	}
	if out, _ := setCronSessionTitle(context.Background(), db, "sess-1", "   "); out != "" {
		t.Fatal(out)
	}
	if out, _ := setCronSessionTitle(context.Background(), nil, "sess-1", "x"); out != "" {
		t.Fatal(out)
	}
}

func TestFailureStreakNudge(t *testing.T) {
	s, _ := newTestStore(t)
	cfg := config.Tree{}
	s.loadConfig = func() (config.Tree, error) { return cfg, nil }
	job := func(streak int, kind string) *Job {
		return &Job{ID: "j1", Name: "scout", FailureStreak: streak, Schedule: &Schedule{Kind: kind}}
	}
	if s.failureStreakNudge(job(0, "cron")) != "" || s.failureStreakNudge(job(1, "cron")) != "" {
		t.Fatal("below threshold")
	}
	if s.failureStreakNudge(job(10, "once")) != "" {
		t.Fatal("one shot nudged")
	}
	if got := s.failureStreakNudge(job(2, "cron")); !strings.Contains(got, "failed 3 runs in a row") || !strings.Contains(got, "`icebath cron pause scout`") {
		t.Fatal(got)
	}
	cfg = config.Tree{"cron": map[string]any{"failure_nudge_threshold": 5}}
	if s.failureStreakNudge(job(3, "cron")) != "" || s.failureStreakNudge(job(4, "cron")) == "" {
		t.Fatal("configured threshold")
	}
	cfg = config.Tree{"cron": map[string]any{"failure_nudge_threshold": 0}}
	if s.failureStreakNudge(job(50, "cron")) != "" {
		t.Fatal("disabled")
	}
	cfg = config.Tree{}
	if s.failureStreakNudge(&Job{ID: "old", Schedule: &Schedule{Kind: "interval"}}) != "" {
		t.Fatal("missing streak")
	}
}

var mcpMergeCfg = config.Tree{"mcp_servers": map[string]any{
	"finnhub":        map[string]any{"enabled": true},
	"playwright":     map[string]any{"enabled": true},
	"disabled_one":   map[string]any{"enabled": false},
	"string_enabled": map[string]any{"enabled": "true"},
	"not_a_dict":     "ignored",
}}

func TestPerJobToolsetMcpMerge(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	got := mergeMCPIntoPerJobToolsets(ctx, []string{"web", "terminal"}, mcpMergeCfg)
	if strings.Join(got, ",") != "web,terminal,finnhub,playwright,string_enabled" {
		t.Fatal(got)
	}
	if got := mergeMCPIntoPerJobToolsets(ctx, []string{"web", "finnhub"}, mcpMergeCfg); strings.Join(got, ",") != "web,finnhub" {
		t.Fatal(got)
	}
	if got := mergeMCPIntoPerJobToolsets(ctx, []string{"web", "no_mcp"}, mcpMergeCfg); strings.Join(got, ",") != "web" {
		t.Fatal(got)
	}
	for _, in := range [][]string{{"web", "mcp-finnhub"}, {"mcp-finnhub", "terminal"}, {"web", "mcp-disabled_one"}, {"web", "disabled_one"}} {
		if got := mergeMCPIntoPerJobToolsets(ctx, in, mcpMergeCfg); strings.Join(got, ",") != strings.Join(in, ",") {
			t.Fatal(in, got)
		}
	}
	got, err := resolveCronEnabledToolsets(ctx, &Job{EnabledToolsets: []string{"memory", "file"}}, config.Tree{"mcp_servers": map[string]any{}})
	if err != nil || strings.Join(got, ",") != "memory,file" {
		t.Fatal(got, err)
	}
	if got, _ := resolveCronEnabledToolsets(ctx, &Job{EnabledToolsets: []string{}}, config.Tree{}); got == nil || len(got) != 0 {
		t.Fatal(got)
	}
	if _, err := resolveCronEnabledToolsets(ctx, &Job{}, config.Tree{"platform_toolsets": "oops"}); err == nil || !strings.Contains(err.Error(), "toolset resolution failed") {
		t.Fatal(err)
	}
	got, err = resolveCronEnabledToolsets(ctx, &Job{EnabledToolsets: []string{"nonexistent_ts"}}, config.Tree{"platform_toolsets": "oops", "mcp_servers": map[string]any{}})
	if err != nil || strings.Join(got, ",") != "nonexistent_ts" {
		t.Fatal(got, err)
	}
	got, err = resolveCronEnabledToolsets(ctx, &Job{}, config.Tree{"platform_toolsets": map[string]any{"cron": []any{"web", "memory", "file"}}})
	if err != nil || strings.Join(got, ",") != "file,memory,web" {
		t.Fatal(got, err)
	}
}

func TestCronDisabledToolsetsAndMarkers(t *testing.T) {
	if got := resolveCronDisabledToolsets(config.Tree{}); strings.Join(got, ",") != "cronjob,messaging,clarify" {
		t.Fatal(got)
	}
	got := resolveCronDisabledToolsets(config.Tree{"cron": map[string]any{"allow_agent_scheduling": true}, "agent": map[string]any{"disabled_toolsets": []any{"web", " messaging "}}})
	if strings.Join(got, ",") != "messaging,clarify,web" {
		t.Fatal(got)
	}
	if ev, ok := cronFailureMarkerError("[CRON_FAILURE]  \nupstream 500\n"); !ok || ev != "upstream 500" {
		t.Fatal(ev, ok)
	}
	if ev, ok := cronFailureMarkerError("[CRON_FAILURE]"); !ok || ev != "Cron agent reported failure." {
		t.Fatal(ev)
	}
	if _, ok := cronFailureMarkerError("ok\n[CRON_FAILURE]"); ok {
		t.Fatal("marker must open the response")
	}
	if b, d, ok := staleCodeYieldLabels((&CronTickYielded{BootRev: "abc", DiskRev: "def"}).Error()); ok {
		t.Fatal(b, d)
	}
	if b, d, ok := staleCodeYieldLabels("CronTickYielded: " + (&CronTickYielded{BootRev: "abc", DiskRev: "def"}).Error()); !ok || b != "abc" || d != "def" {
		t.Fatal(b, d, ok)
	}
}

func TestReasoningConfigPinAndFallback(t *testing.T) {
	s, buf := newTestStore(t)
	if got := s.resolveJobReasoningConfig(&Job{ID: "j", ReasoningEffort: strp("high")}, config.Tree{}, "m"); got == nil || got["effort"] != "high" {
		t.Fatal(got)
	}
	s.resolveJobReasoningConfig(&Job{ID: "j", ReasoningEffort: strp("bogus")}, config.Tree{}, "m")
	if !strings.Contains(buf.String(), "invalid stored reasoning_effort") {
		t.Fatal(buf.String())
	}
}

func withCodeSkew(t *testing.T, boot, disk string) {
	t.Helper()
	prevSkew, prevSHA := detectGatewayCodeSkew, currentGatewayCodeSHA
	detectGatewayCodeSkew = func() (string, string, bool) { return boot[:10], disk[:10], true }
	currentGatewayCodeSHA = func() (string, bool) { return disk, true }
	t.Cleanup(func() { detectGatewayCodeSkew, currentGatewayCodeSHA = prevSkew, prevSHA })
}

func TestPerProfileYieldGate(t *testing.T) {
	t.Run("yield gate is answered per served profile", func(t *testing.T) {
		a, b := ownershipHomes(t)
		boot, disk := strings.Repeat("a", 40), strings.Repeat("b", 40)
		withCodeSkew(t, boot, disk)
		foreignGateway(map[string]any{"pid": int64(4321), "code_sha": disk, "hermes_home": a})
		registerTickedHomes([]string{a, b})
		if _, _, ok := openCronStore(a, nil).shouldYieldTickToFreshGateway(); !ok {
			t.Fatal("profile A must yield to the fresher gateway serving it")
		}
		if _, _, ok := openCronStore(b, nil).shouldYieldTickToFreshGateway(); ok {
			t.Fatal("profile B has no fresher ticker; yielding would stop its cron entirely")
		}
	})
	t.Run("host multiplexer never yields a home it ticks", func(t *testing.T) {
		a, b := ownershipHomes(t)
		boot, disk := strings.Repeat("a", 40), strings.Repeat("b", 40)
		withCodeSkew(t, boot, disk)
		gatewayProbes.ownsRuntimeLock = func() bool { return true }
		registerTickedHomes([]string{a})
		if !ownsCronTickFor(a) {
			t.Fatal("A must be owned")
		}
		if _, _, ok := openCronStore(a, nil).shouldYieldTickToFreshGateway(); ok {
			t.Fatal("owned home yielded")
		}
		if ownsCronTickFor(b) {
			t.Fatal("B must not be owned")
		}
	})
}
