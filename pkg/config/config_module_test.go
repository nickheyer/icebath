package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Seeded default every install between its introduction and the rewrite carried, kept as a literal
const preRewriteDefaultSoul = "You are Icebath Agent, an intelligent AI assistant created by Nous " +
	"Research. You are helpful, knowledgeable, and direct. You assist " +
	"users with a wide range of tasks including answering questions, " +
	"writing and editing code, analyzing information, creative work, " +
	"and executing actions via your tools. You communicate clearly, " +
	"admit uncertainty when appropriate, and prioritize being " +
	"genuinely useful over being verbose unless otherwise directed " +
	"below. Be targeted and efficient in your exploration and " +
	"investigations."

// Facade home whose parse failure warnings land in the returned buffer
func moduleHome(t *testing.T, configText string) (string, string, *bytes.Buffer) {
	t.Helper()
	h, path := facadeHome(t, configText)
	errOut := &bytes.Buffer{}
	prev := ParseFailureOut
	ParseFailureOut = errOut
	t.Cleanup(func() { ParseFailureOut = prev })
	return h, path, errOut
}

func moduleLoad(t *testing.T, h string) Tree {
	t.Helper()
	cfg, err := LoadContext(t.Context(), h, facadeLookup)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func moduleSave(t *testing.T, h string, cfg map[string]any) {
	t.Helper()
	if err := SaveConfig(t.Context(), h, cfg, SaveOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Parsed mapping of a YAML file on disk
func moduleYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := SafeLoad(data)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := doc.(map[string]any)
	return m
}

// Writes data as the safe dump of a mapping
func moduleDump(t *testing.T, path string, data map[string]any) {
	t.Helper()
	out, err := SafeDump(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Corrupt snapshots the parse failure path left under the home
func corruptBackups(t *testing.T, h string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(h, "backups", "config", "config.yaml.corrupt.*"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func moduleRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestGetHermesHomeDefaultPath(t *testing.T) {
	t.Setenv("ICEBATH_HOME", "")
	os.Unsetenv("ICEBATH_HOME")
	if got, want := home.Dir(t.Context()), home.PlatformDefault(); got != want {
		t.Fatalf("home %q, want the platform default %q", got, want)
	}
}

func TestEnsureHermesHomeCreatesDefaultSoulMdIfMissing(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := EnsureIcebathHome(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if text := moduleRead(t, filepath.Join(h, "SOUL.md")); strings.TrimSpace(text) == "" {
		t.Fatal("seeded SOUL.md is blank")
	}
}

func TestEnsureHermesHomeUpgradesLegacyTemplateSoulMd(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	soul := filepath.Join(h, "SOUL.md")
	if err := os.WriteFile(soul, []byte(legacyTemplateSouls[0]+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureIcebathHome(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if got := moduleRead(t, soul); got != DefaultIdentity {
		t.Fatalf("scaffold not upgraded: %q", got)
	}
}

func TestEnsureHermesHomeUpgradesPreRewriteDefaultSoulMd(t *testing.T) {
	if preRewriteDefaultSoul == DefaultIdentity {
		t.Fatal("fixture must predate the rewrite")
	}
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	soul := filepath.Join(h, "SOUL.md")
	if err := os.WriteFile(soul, []byte(preRewriteDefaultSoul), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureIcebathHome(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if got := moduleRead(t, soul); got != DefaultIdentity {
		t.Fatalf("pre rewrite default not upgraded: %q", got)
	}
}

func TestEnsureHermesHomeDoesNotUpgradeUserCustomizedSoulMd(t *testing.T) {
	customized := preRewriteDefaultSoul + " Also: always answer in rhyming couplets."
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	soul := filepath.Join(h, "SOUL.md")
	if err := os.WriteFile(soul, []byte(customized), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureIcebathHome(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if got := moduleRead(t, soul); got != customized || got == DefaultIdentity {
		t.Fatalf("customized soul rewritten: %q", got)
	}
}

func TestLegacyTemplateSoulsCoverEveryEarlierSeed(t *testing.T) {
	if len(legacyTemplateSouls) != 5 {
		t.Fatalf("%d legacy texts", len(legacyTemplateSouls))
	}
	for i, text := range legacyTemplateSouls {
		if !IsLegacyTemplateSoul("\uFEFF\uFEFF" + strings.ReplaceAll(text, "\n", "\r\n") + "\x1f\n") {
			t.Fatalf("legacy text %d not recognised", i)
		}
		if IsLegacyTemplateSoul(text + " x") {
			t.Fatalf("edited legacy text %d recognised", i)
		}
	}
	if strings.Contains(legacyTemplateSouls[3], "\u2014") || !strings.Contains(legacyTemplateSouls[3], "the ask -- a one-line") {
		t.Fatalf("ASCII dashed variant %q", legacyTemplateSouls[3])
	}
	if !strings.HasPrefix(legacyTemplateSouls[4], "You are icebath, an agent that lives in the terminal.") {
		t.Fatalf("earlier icebath seed %q", legacyTemplateSouls[4])
	}
	if IsLegacyTemplateSoul(DefaultIdentity) {
		t.Fatal("the current default counts as legacy")
	}
}

func TestLoadConfigDefaultsReturnsDefaultsWhenNoFile(t *testing.T) {
	h, _, _ := moduleHome(t, "")
	cfg := moduleLoad(t, h)
	defaults := Defaults()
	if !reflect.DeepEqual(cfg["model"], defaults["model"]) {
		t.Fatalf("model %v, want %v", cfg["model"], defaults["model"])
	}
	if !pyEq(cfg.Get("agent.max_turns"), defaults.Get("agent.max_turns")) {
		t.Fatalf("agent.max_turns %v", cfg.Get("agent.max_turns"))
	}
	if _, has := cfg["max_turns"]; has {
		t.Fatal("root max_turns present")
	}
	if _, has := cfg["terminal"]; !has || cfg.Get("terminal.backend") != "local" {
		t.Fatalf("terminal %v", cfg["terminal"])
	}
	if cfg.Get("display.interim_assistant_messages") != true {
		t.Fatalf("interim_assistant_messages %v", cfg.Get("display.interim_assistant_messages"))
	}
}

func TestLoadConfigDefaultsLegacyRootLevelMaxTurnsMigratesToAgentConfig(t *testing.T) {
	h, _, _ := moduleHome(t, "max_turns: 42\n")
	cfg := moduleLoad(t, h)
	if !pyEq(cfg.Get("agent.max_turns"), 42) {
		t.Fatalf("agent.max_turns %v", cfg.Get("agent.max_turns"))
	}
	if _, has := cfg["max_turns"]; has {
		t.Fatal("root max_turns kept")
	}
}

func TestLoadConfigParseFailureCorruptConfigIsBackedUp(t *testing.T) {
	broken := "\tmodel: test/custom\nbroken indent:\n"
	h, path, errOut := moduleHome(t, broken)
	moduleLoad(t, h)
	baks := corruptBackups(t, h)
	if len(baks) != 1 {
		t.Fatalf("expected one backup, got %v", baks)
	}
	if got := moduleRead(t, baks[0]); got != broken {
		t.Fatalf("backup content %q", got)
	}
	if got := moduleRead(t, path); got != broken {
		t.Fatalf("config.yaml rewritten: %q", got)
	}
	if !strings.Contains(errOut.String(), baks[0]) {
		t.Fatalf("stderr does not name the backup: %q", errOut.String())
	}
}

func TestLoadConfigParseFailureLastKnownGoodRetainedWithinProcess(t *testing.T) {
	h, path, errOut := moduleHome(t, "model:\n  default: test/custom-model\napprovals:\n  deny:\n    - 'curl*evil.com*'\n")
	good := moduleLoad(t, h)
	if good.Get("model.default") != "test/custom-model" || !reflect.DeepEqual(good.Get("approvals.deny"), []any{"curl*evil.com*"}) {
		t.Fatalf("good config %v %v", good.Get("model.default"), good.Get("approvals.deny"))
	}
	errOut.Reset()
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte("approvals:\n  deny: [unclosed\n  :::bad {{{\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := moduleLoad(t, h)
	if after.Get("model.default") != "test/custom-model" || !reflect.DeepEqual(after.Get("approvals.deny"), []any{"curl*evil.com*"}) {
		t.Fatalf("last good config dropped: %v %v", after.Get("model.default"), after.Get("approvals.deny"))
	}
	if !strings.Contains(errOut.String(), "settings it loaded before the edit") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestEmptyConfigSectionsNullSectionKeepsDefaultsInLoadConfig(t *testing.T) {
	h, _, _ := moduleHome(t, "model:\n  default: test/custom\nterminal:\ndisplay:\n")
	cfg := moduleLoad(t, h)
	if cfg.Get("model.default") != "test/custom" {
		t.Fatalf("model.default %v", cfg.Get("model.default"))
	}
	terminal, ok := cfg["terminal"].(map[string]any)
	if !ok || !reflect.DeepEqual(terminal, Defaults()["terminal"]) {
		t.Fatalf("terminal %v", cfg["terminal"])
	}
	if _, ok := cfg["display"].(map[string]any); !ok {
		t.Fatalf("display %T", cfg["display"])
	}
}

func TestEmptyConfigSectionsNullOverrideOfNonDictDefaultStillApplies(t *testing.T) {
	merged := DeepMerge(map[string]any{"scalar": 5, "section": map[string]any{"a": 1}}, map[string]any{"scalar": nil, "section": nil})
	if v, has := merged["scalar"]; !has || v != nil {
		t.Fatalf("scalar %v", merged["scalar"])
	}
	if !reflect.DeepEqual(merged["section"], map[string]any{"a": 1}) {
		t.Fatalf("section %v", merged["section"])
	}
}

func TestSaveAndLoadRoundtripSaveConfigRefusesToOverwriteUnreadableExistingConfig(t *testing.T) {
	original := "model: test/original\n"
	h, path, _ := moduleHome(t, original)
	denyConfigReads(t, path)
	err := SaveConfig(t.Context(), h, map[string]any{"model": "test/replacement"}, SaveOptions{})
	if err == nil || !isRuntimeError(err) || !strings.Contains(err.Error(), "this change was not saved") {
		t.Fatalf("refusal %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := moduleRead(t, path); got != original {
		t.Fatalf("config changed: %q", got)
	}
}

func TestSaveAndLoadRoundtripAtomicConfigWriteRefusesUnparseableExistingConfig(t *testing.T) {
	original := "broken: [unterminated\n"
	h, path, _ := moduleHome(t, original)
	err := AtomicConfigWrite(t.Context(), h, path, map[string]any{"model": map[string]any{"provider": "openai"}}, "")
	if err == nil || !isRuntimeError(err) || !strings.Contains(err.Error(), "formatting error") {
		t.Fatalf("refusal %v", err)
	}
	if got := moduleRead(t, path); got != original {
		t.Fatalf("config changed: %q", got)
	}
	if len(corruptBackups(t, h)) == 0 {
		t.Fatal("no corrupt backup")
	}
}

func TestSaveAndLoadRoundtripAtomicConfigWriteRefusesPartialStateInsteadOfWipingConfig(t *testing.T) {
	h, path, _ := moduleHome(t, "")
	original := map[string]any{}
	for i := range 99 {
		original[fmt.Sprintf("k%d", i)] = i
	}
	moduleDump(t, path, original)
	err := AtomicConfigWrite(t.Context(), h, path, map[string]any{"skills": map[string]any{"disabled": []any{"a"}}}, "")
	if err == nil || !isRuntimeError(err) || !strings.Contains(err.Error(), "would lose settings omitted") {
		t.Fatalf("refusal %v", err)
	}
	if got := moduleYAML(t, path); !pyEq(got, original) {
		t.Fatalf("document changed: %v", got)
	}
}

func TestSaveAndLoadRoundtripAtomicConfigWriteRefusesNestedOmissionsWithSameTopLevelKeys(t *testing.T) {
	h, path, _ := moduleHome(t, "")
	original := map[string]any{
		"plugins": map[string]any{"enabled": []any{"guard"}, "disabled": []any{}, "config": map[string]any{"guard": map[string]any{"mode": "strict"}}},
		"model":   map[string]any{"default": "gpt-5"},
	}
	moduleDump(t, path, original)
	err := AtomicConfigWrite(t.Context(), h, path, map[string]any{"plugins": map[string]any{"disabled": []any{"legacy"}}, "model": map[string]any{"default": "gpt-5"}}, "")
	if err == nil || !isRuntimeError(err) || !regexp.MustCompile(`plugins\.(enabled|config)`).MatchString(err.Error()) {
		t.Fatalf("refusal %v", err)
	}
	if got := moduleYAML(t, path); !pyEq(got, original) {
		t.Fatalf("document changed: %v", got)
	}
}

func TestSaveAndLoadRoundtripAtomicConfigReplaceMakesDeleteByOmissionExplicit(t *testing.T) {
	h, path, _ := moduleHome(t, "")
	moduleDump(t, path, map[string]any{"model": map[string]any{"default": "gpt-5"}, "plugins": map[string]any{"enabled": []any{"guard"}}})
	replacement := map[string]any{"model": map[string]any{"default": "gpt-5"}}
	if err := AtomicConfigReplace(t.Context(), h, path, replacement, ""); err != nil {
		t.Fatal(err)
	}
	if got := moduleYAML(t, path); !pyEq(got, replacement) {
		t.Fatalf("document %v", got)
	}
}

func TestLoadEnvInlineCommentsUnquotedHashIsACommentQuotedHashIsData(t *testing.T) {
	h, _, _ := moduleHome(t, "")
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("PASSWORD=abc #123\nPASSWORD2=\"abc #123\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	InvalidateEnvCache()
	env := LoadEnv(h)
	if env["PASSWORD"] != "abc" || env["PASSWORD2"] != "abc #123" {
		t.Fatalf("env %q", env)
	}
}

// Restores a process env var the writers under test publish into
func isolateProcessEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		prev, had := os.LookupEnv(n)
		os.Unsetenv(n)
		t.Cleanup(func() {
			if had {
				os.Setenv(n, prev)
			} else {
				os.Unsetenv(n)
			}
		})
	}
}

func TestSaveEnvValueSecureSaveEnvValuePreservesExistingFileModeOnPosix(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	h, _, _ := moduleHome(t, "")
	isolateProcessEnv(t, "TENOR_API_KEY")
	envPath := filepath.Join(h, ".env")
	if err := os.WriteFile(envPath, []byte("EXISTING=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(envPath, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := SaveEnvValueContext(t.Context(), h, "TENOR_API_KEY", "sk-test-secret"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(envPath)
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v %v, want 0640", st.Mode().Perm(), err)
	}
}

func TestSaveEnvValueSecureSaveEnvValueQuotesValuesContainingHash(t *testing.T) {
	h, _, _ := moduleHome(t, "")
	isolateProcessEnv(t, "ANTHROPIC_TOKEN")
	token := "sk-ant-oat01-abc#xyz#more"
	if err := SaveEnvValueContext(t.Context(), h, "ANTHROPIC_TOKEN", token); err != nil {
		t.Fatal(err)
	}
	content := moduleRead(t, filepath.Join(h, ".env"))
	if !strings.Contains(content, `ANTHROPIC_TOKEN="`+token+`"`) {
		t.Fatalf("content %q", content)
	}
	if parsed := ParseEnv(content); parsed["ANTHROPIC_TOKEN"] != token {
		t.Fatalf("parsed %q", parsed)
	}
	if got := LoadEnv(h)["ANTHROPIC_TOKEN"]; got != token {
		t.Fatalf("load_env %q", got)
	}
}

func TestSaveEnvValueSecureSaveEnvValueAlreadyQuotedInputIsNotDoubleWrappedIdempotently(t *testing.T) {
	h, _, _ := moduleHome(t, "")
	isolateProcessEnv(t, "TERMINAL_SSH_KEY")
	raw := `"/Users/me/Application Support/key"`
	envPath := filepath.Join(h, ".env")
	if err := SaveEnvValueContext(t.Context(), h, "TERMINAL_SSH_KEY", raw); err != nil {
		t.Fatal(err)
	}
	first := moduleRead(t, envPath)
	if err := SaveEnvValueContext(t.Context(), h, "TERMINAL_SSH_KEY", raw); err != nil {
		t.Fatal(err)
	}
	if second := moduleRead(t, envPath); first != second {
		t.Fatalf("resave changed the file:\n%q\n%q", first, second)
	}
	line := ""
	for _, ln := range strings.Split(first, "\n") {
		if strings.HasPrefix(ln, "TERMINAL_SSH_KEY=") {
			line = ln
			break
		}
	}
	if !strings.HasPrefix(line, `TERMINAL_SSH_KEY="`) || !strings.HasSuffix(line, `"`) || strings.Count(line, `TERMINAL_SSH_KEY="`) != 1 {
		t.Fatalf("line %q", line)
	}
	if got := LoadEnv(h)["TERMINAL_SSH_KEY"]; got != raw {
		t.Fatalf("load_env %q, want %q", got, raw)
	}
}

func TestRemoveEnvValueRemovesKeyFromEnvFile(t *testing.T) {
	h, _, _ := moduleHome(t, "")
	envPath := filepath.Join(h, ".env")
	if err := os.WriteFile(envPath, []byte("KEY_A=value_a\nKEY_B=value_b\nKEY_C=value_c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KEY_B", "value_b")
	removed, err := RemoveHomeEnvValue(t.Context(), h, "KEY_B")
	if err != nil || !removed {
		t.Fatalf("removed %v %v", removed, err)
	}
	content := moduleRead(t, envPath)
	if strings.Contains(content, "KEY_B") || !strings.Contains(content, "KEY_A=value_a") || !strings.Contains(content, "KEY_C=value_c") {
		t.Fatalf("content %q", content)
	}
}

func TestRemoveEnvValueClearsOsEnvironEvenWhenNotInFile(t *testing.T) {
	h, _, _ := moduleHome(t, "")
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("OTHER=stuff\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORPHAN_KEY", "orphan")
	if _, err := RemoveHomeEnvValue(t.Context(), h, "ORPHAN_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, has := os.LookupEnv("ORPHAN_KEY"); has {
		t.Fatal("ORPHAN_KEY still in the process env")
	}
}

func TestRemoveEnvValueRemoveEnvValuePreservesExistingFileModeOnPosix(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	h, _, _ := moduleHome(t, "")
	envPath := filepath.Join(h, ".env")
	if err := os.WriteFile(envPath, []byte("KEEP=value\nDROP=gone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(envPath, 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROP", "gone")
	removed, err := RemoveHomeEnvValue(t.Context(), h, "DROP")
	if err != nil || !removed {
		t.Fatalf("removed %v %v", removed, err)
	}
	if strings.Contains(moduleRead(t, envPath), "DROP") {
		t.Fatal("DROP kept")
	}
	st, err := os.Stat(envPath)
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v %v, want 0640", st.Mode().Perm(), err)
	}
}

// Fails every temp file flush with a full disk the way a writer dying before the replace does
func failConfigFlush(t *testing.T) {
	t.Helper()
	prev := fsyncFile
	fsyncFile = func(f *os.File) error { return &os.PathError{Op: "write", Path: f.Name(), Err: syscall.ENOSPC} }
	t.Cleanup(func() { fsyncFile = prev })
}

func TestSaveConfigAtomicityNoPartialWriteOnCrash(t *testing.T) {
	h, path, _ := moduleHome(t, "")
	cfg := moduleLoad(t, h)
	cfg["model"] = "original-model"
	moduleSave(t, h, cfg)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	prev := fsyncFile
	failConfigFlush(t)
	cfg["model"] = "should-not-persist"
	err := SaveConfig(t.Context(), h, cfg, SaveOptions{})
	fsyncFile = prev
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("crash surfaced as %v", err)
	}
	if got := moduleLoad(t, h)["model"]; got != "original-model" {
		t.Fatalf("model %v", got)
	}
}

func TestSaveConfigAtomicityNoLeftoverTempFiles(t *testing.T) {
	h, _, _ := moduleHome(t, "")
	cfg := moduleLoad(t, h)
	moduleSave(t, h, cfg)
	failConfigFlush(t)
	if err := SaveConfig(t.Context(), h, cfg, SaveOptions{}); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("crash surfaced as %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(h, ".*config*.tmp")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestSaveConfigAtomicityAtomicWriteCreatesValidYaml(t *testing.T) {
	h, path, _ := moduleHome(t, "")
	cfg := moduleLoad(t, h)
	cfg["model"] = "test/atomic-model"
	cfg.Map("agent")["max_turns"] = 77
	moduleSave(t, h, cfg)
	raw := moduleYAML(t, path)
	if raw["model"] != "test/atomic-model" || !pyEq(Tree(raw).Get("agent.max_turns"), 77) {
		t.Fatalf("raw %v", raw)
	}
}

func TestSanitizeEnvLinesSanitizeEnvFileDoesNotRewriteValueSemantics(t *testing.T) {
	h, _, _ := moduleHome(t, "")
	envPath := filepath.Join(h, ".env")
	text := "FAL_KEY=good\nOPENROUTER_API_KEY=valFIRECRAWL_API_KEY=val2\n"
	if err := os.WriteFile(envPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	fixes, err := SanitizeEnvFile(t.Context(), h)
	if err != nil || fixes != 0 {
		t.Fatalf("fixes %d %v", fixes, err)
	}
	if got := moduleRead(t, envPath); got != text {
		t.Fatalf("content %q", got)
	}
}

func TestSanitizeEnvLinesSanitizeEnvFileNoopOnCleanFile(t *testing.T) {
	h, _, _ := moduleHome(t, "")
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("GOOD_KEY=good\nOTHER_KEY=other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fixes, err := SanitizeEnvFile(t.Context(), h); err != nil || fixes != 0 {
		t.Fatalf("fixes %d %v", fixes, err)
	}
}

func TestOptionalEnvVarsRegistryMaxIterationsNotOfferedAsEnvVar(t *testing.T) {
	if OptionalEnvVars.Has("ICEBATH_MAX_ITERATIONS") {
		t.Fatal("ICEBATH_MAX_ITERATIONS is offered as an editable env var")
	}
}

func TestConfigSupportFloorFloorMessageUsesDisplayHermesHome(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if msg := SupportFloorMessage(h); !strings.Contains(msg, home.Display(h)+"/config.yaml") {
		t.Fatalf("message %q", msg)
	}
}
