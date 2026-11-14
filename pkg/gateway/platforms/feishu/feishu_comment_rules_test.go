package feishu

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

func strp(s string) *string { return &s }

func TestParseFullDocumentRule(t *testing.T) {
	raw, _ := mustObject(t, `{"enabled": false, "policy": "allowlist", "allow_from": ["ou_a", "ou_b"]}`)
	rule := parseDocumentRule(raw)
	if rule.Enabled == nil || *rule.Enabled {
		t.Fatalf("enabled = %v", rule.Enabled)
	}
	if rule.Policy == nil || *rule.Policy != "allowlist" {
		t.Fatalf("policy = %v", rule.Policy)
	}
	if !reflect.DeepEqual(rule.AllowFrom, NewStringSet("ou_a", "ou_b")) {
		t.Fatalf("allow_from = %v", rule.AllowFrom)
	}
}

func TestResolveRuleExactMatch(t *testing.T) {
	cfg := CommentsConfig{Enabled: true, Policy: "pairing", AllowFrom: NewStringSet("ou_top"),
		Documents: map[string]CommentDocumentRule{"docx:abc": {Policy: strp("allowlist")}}}
	rule := ResolveRule(cfg, "docx", "abc", "")
	if rule.Policy != "allowlist" || rule.MatchSource[:6] != "exact:" {
		t.Fatalf("rule = %+v", rule)
	}
}

func TestResolveRuleWildcardMatch(t *testing.T) {
	cfg := CommentsConfig{Enabled: true, Policy: "pairing", AllowFrom: StringSet{},
		Documents: map[string]CommentDocumentRule{"*": {Policy: strp("allowlist")}}}
	rule := ResolveRule(cfg, "docx", "unknown", "")
	if rule.Policy != "allowlist" || rule.MatchSource != "wildcard" {
		t.Fatalf("rule = %+v", rule)
	}
}

func TestResolveRuleTopLevelFallback(t *testing.T) {
	cfg := CommentsConfig{Enabled: true, Policy: "pairing", AllowFrom: NewStringSet("ou_top"), Documents: map[string]CommentDocumentRule{}}
	rule := ResolveRule(cfg, "docx", "whatever", "")
	if rule.Policy != "pairing" || !reflect.DeepEqual(rule.AllowFrom, NewStringSet("ou_top")) || rule.MatchSource != "top" {
		t.Fatalf("rule = %+v", rule)
	}
}

func TestHasWikiKeysWithoutWikiKeys(t *testing.T) {
	cfg := DefaultCommentsConfig()
	cfg.Documents = map[string]CommentDocumentRule{"docx:abc": {Policy: strp("allowlist")}, "*": {Policy: strp("pairing")}}
	if HasWikiKeys(cfg) {
		t.Fatal("unexpected wiki key")
	}
}

func TestIsUserAllowedAllowlistAllowsListed(t *testing.T) {
	ok, err := IsUserAllowed(context.Background(), ResolvedCommentRule{true, "allowlist", NewStringSet("ou_a"), "top"}, "ou_a")
	if err != nil || !ok {
		t.Fatalf("allowed = %v, %v", ok, err)
	}
}

func TestIsUserAllowedPairingChecksStore(t *testing.T) {
	orig := loadPairingApproved
	loadPairingApproved = func(context.Context) (StringSet, error) { return NewStringSet("ou_approved"), nil }
	defer func() { loadPairingApproved = orig }()
	rule := ResolvedCommentRule{true, "pairing", StringSet{}, "top"}
	if ok, _ := IsUserAllowed(context.Background(), rule, "ou_approved"); !ok {
		t.Fatal("approved user denied")
	}
	if ok, _ := IsUserAllowed(context.Background(), rule, "ou_unknown"); ok {
		t.Fatal("unknown user allowed")
	}
}

func TestMtimeCacheReturnsEmptyDictForMissingFile(t *testing.T) {
	data, err := NewMtimeCache("/nonexistent/path.json").Load(context.Background())
	if err != nil || data.Len() != 0 {
		t.Fatalf("load = %v, %v", data, err)
	}
}

func TestLoadConfigWithDocuments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	writeFile(t, path, `{"enabled": true, "policy": "allowlist", "allow_from": ["ou_a"], "documents": {"*": {"policy": "pairing"}, "docx:abc": {"policy": "allowlist", "allow_from": ["ou_b"]}}}`)
	origFile, origCache := RulesFile, rulesCache
	RulesFile, rulesCache = path, NewMtimeCache(path)
	cfg, err := LoadConfig(context.Background())
	RulesFile, rulesCache = origFile, origCache
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled != true || cfg.Policy != "allowlist" || !reflect.DeepEqual(cfg.AllowFrom, NewStringSet("ou_a")) {
		t.Fatalf("cfg = %+v", cfg)
	}
	if _, ok := cfg.Documents["*"]; !ok {
		t.Fatal("wildcard rule missing")
	}
	if doc, ok := cfg.Documents["docx:abc"]; !ok || *doc.Policy != "allowlist" {
		t.Fatalf("docx:abc = %+v", doc)
	}
}

func TestPairingStoreAddAndList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairing.json")
	writeFile(t, path, `{"approved": {}}`)
	origFile, origCache := PairingFile, pairingCache
	PairingFile, pairingCache = path, NewMtimeCache(path)
	defer func() { PairingFile, pairingCache = origFile, origCache }()
	ctx := context.Background()
	if added, err := PairingAdd(ctx, "ou_new"); err != nil || !added {
		t.Fatalf("add = %v, %v", added, err)
	}
	approved, err := PairingList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := approved.Get("ou_new"); !ok {
		t.Fatalf("approved = %v", approved.Keys)
	}
}

func TestRulesAndPairingFollowHomeOverride(t *testing.T) {
	tmp := t.TempDir()
	profA, profB := filepath.Join(tmp, "A"), filepath.Join(tmp, "B")
	for _, p := range []struct {
		dir, body string
	}{{profA, `{"enabled": true}`}, {profB, `{"enabled": false}`}} {
		if err := os.Mkdir(p.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(p.dir, "feishu_comment_rules.json"), p.body)
	}
	under := func(dir string) context.Context { return home.WithOverride(context.Background(), dir) }
	enabled := func(dir string) any {
		cfg, err := LoadConfig(under(dir))
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Enabled
	}
	if enabled(profA) != true || enabled(profB) != false || enabled(profA) != true {
		t.Fatal("rules did not follow the profile override")
	}
	if added, err := PairingAdd(under(profB), "ou_b"); err != nil || !added {
		t.Fatalf("add = %v, %v", added, err)
	}
	if _, err := os.Stat(filepath.Join(profB, "feishu_comment_pairing.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(profA, "feishu_comment_pairing.json")); err == nil {
		t.Fatal("profile A gained a pairing file")
	}
	approved, err := PairingList(under(profA))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := approved.Get("ou_b"); ok {
		t.Fatal("profile A sees profile B approval")
	}
}

func TestMainCheckAndPairingCommands(t *testing.T) {
	dir := t.TempDir()
	ctx := home.WithOverride(context.Background(), dir)
	writeFile(t, filepath.Join(dir, "feishu_comment_rules.json"), `{"policy": "allowlist", "allow_from": ["ou_b", "ou_a"], "documents": {"docx:x": {"enabled": false}, "*": {}}}`)
	var out strings.Builder
	run := func(args ...string) int {
		out.Reset()
		code, err := Main(ctx, args, &out)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	if run("check", "docx:x", "ou_a") != 0 || out.String() != "Document:     docx:x\nUser:         ou_a\nResolved rule:\n  enabled:      False\n  policy:       allowlist\n  allow_from:   ['ou_a', 'ou_b']\n  match_source: exact:docx:x\nResult:       ALLOWED\n" {
		t.Fatalf("check output %q", out.String())
	}
	if run("pairing", "add", "ou_z") != 0 || out.String() != "Added: ou_z\n" {
		t.Fatalf("pairing add output %q", out.String())
	}
	if run("pairing", "add", "ou_z") != 0 || out.String() != "Already approved: ou_z\n" {
		t.Fatalf("pairing add again output %q", out.String())
	}
	if run("pairing", "remove") != 1 || out.String() != "Usage: pairing remove <user_open_id>\n" {
		t.Fatalf("pairing remove usage %q", out.String())
	}
	if run("status") != 0 || !strings.Contains(out.String(), "Document rules (2):\n  [*] (empty — inherits all)\n  [docx:x] enabled=False\n\nPairing approved (1):\n  ou_z  (approved_at=") {
		t.Fatalf("status output %q", out.String())
	}
	if run("bogus") != 1 || !strings.HasPrefix(out.String(), "Unknown command: bogus\nUsage: python -m") {
		t.Fatalf("unknown command output %q", out.String())
	}
}

func TestCommentTurnRuleReadsFollowRoutedAndLaunchScopes(t *testing.T) {
	tmp := t.TempDir()
	routed, launch := filepath.Join(tmp, "profiles", "b"), filepath.Join(tmp, "launch")
	for _, dir := range []string{routed, launch} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(routed, "feishu_comment_rules.json"), `{"enabled": true, "policy": "allowlist"}`)
	writeFile(t, filepath.Join(routed, "feishu_comment_pairing.json"), `{"approved": {"ou_user": {}}}`)
	writeFile(t, filepath.Join(launch, "feishu_comment_rules.json"),
		`{"enabled": true, "policy": "pairing", "documents": {"docx:docx_t": {"policy": "allowlist", "allow_from": ["ou_other"]}}}`)
	writeFile(t, filepath.Join(launch, "feishu_comment_pairing.json"), `{"approved": {"ou_user": {}}}`)
	t.Setenv("ICEBATH_HOME", launch)
	turn := func(ctx context.Context) (ResolvedCommentRule, bool, bool) {
		cfg, err := LoadConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rule := ResolveRule(cfg, "docx", "docx_t", "")
		allowed, err := IsUserAllowed(ctx, rule, "ou_user")
		if err != nil {
			t.Fatal(err)
		}
		return rule, HasWikiKeys(cfg), allowed
	}
	routedCtx := home.WithOverride(context.Background(), routed)
	rule, wiki, allowed := turn(routedCtx)
	want := ResolvedCommentRule{Enabled: true, Policy: "allowlist", AllowFrom: StringSet{}, MatchSource: "top"}
	if !reflect.DeepEqual(rule, want) || wiki || allowed {
		t.Fatalf("routed turn = %#v wiki=%v allowed=%v", rule, wiki, allowed)
	}
	rule, wiki, allowed = turn(context.Background())
	want = ResolvedCommentRule{Enabled: true, Policy: "allowlist", AllowFrom: NewStringSet("ou_other"), MatchSource: "exact:docx:docx_t"}
	if !reflect.DeepEqual(rule, want) || wiki || allowed {
		t.Fatalf("launch turn = %#v wiki=%v allowed=%v", rule, wiki, allowed)
	}
	writeFile(t, filepath.Join(routed, "feishu_comment_rules.json"), `{"enabled": true, "policy": "pairing"}`)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(routed, "feishu_comment_rules.json"), future, future); err != nil {
		t.Fatal(err)
	}
	if rule, _, allowed = turn(routedCtx); rule.Policy != "pairing" || !allowed {
		t.Fatalf("routed pairing turn = %#v allowed=%v", rule, allowed)
	}
}
