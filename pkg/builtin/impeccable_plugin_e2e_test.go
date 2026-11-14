package builtin

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	pluginE2EMarketplace = "impeccable-e2e"
	pluginE2ERef         = "impeccable@" + pluginE2EMarketplace
)

// Outcome of installing the embedded plugin subtree into a sandboxed Claude Code, shared by the loader cases
var pluginE2E struct {
	once        sync.Once
	unavailable bool
	details     string
	shipped     []string
	err         error
}

// Output of one claude invocation run in the sandbox with a neutral cwd and isolated config and home
func runClaude(workDir, configDir string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := claudeCommand(ctx, args...)
	if workDir != "" {
		cmd.Dir = workDir
		cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+configDir, "HOME="+workDir, "USERPROFILE="+workDir,
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1")
	}
	out, err := cmd.Output()
	return string(out), err
}

func installPluginE2E() {
	if _, err := runClaude("", "", 30*time.Second, "--version"); err != nil {
		pluginE2E.unavailable = true
		return
	}
	workDir, err := os.MkdirTemp("", "impeccable-plugin-e2e-")
	if err != nil {
		pluginE2E.err = err
		return
	}
	defer os.RemoveAll(workDir)
	fail := func(err error) bool {
		if err != nil && pluginE2E.err == nil {
			pluginE2E.err = err
		}
		return err != nil
	}
	tree, err := materializeAt(filepath.Join(workDir, "cache"))
	if fail(err) {
		return
	}
	pluginDir := tree.Payload("impeccable/plugin")
	configDir := filepath.Join(workDir, "claude-config")
	marketplaceDir := filepath.Join(workDir, "marketplace")
	if fail(os.MkdirAll(configDir, 0o777)) || fail(copyDirSync(pluginDir, filepath.Join(marketplaceDir, "impeccable"))) {
		return
	}
	manifest := jsonStringify(pyjson.ObjectOf(
		"name", pluginE2EMarketplace,
		"owner", pyjson.ObjectOf("name", "impeccable plugin E2E"),
		"plugins", []any{pyjson.ObjectOf("name", "impeccable", "source", "./impeccable", "description", "committed ./plugin subtree")}), "  ")
	if fail(writeBuildFile(filepath.Join(marketplaceDir, ".claude-plugin", "marketplace.json"), manifest)) {
		return
	}
	names, err := markdownNames(filepath.Join(pluginDir, "agents"))
	if fail(err) {
		return
	}
	for _, name := range names {
		pluginE2E.shipped = append(pluginE2E.shipped, strings.TrimSuffix(name, ".md"))
	}
	if _, err := runClaude(workDir, configDir, 120*time.Second, "plugin", "marketplace", "add", marketplaceDir); fail(err) {
		return
	}
	if _, err := runClaude(workDir, configDir, 120*time.Second, "plugin", "install", pluginE2ERef); fail(err) {
		return
	}
	pluginE2E.details, err = runClaude(workDir, configDir, 120*time.Second, "plugin", "details", pluginE2ERef)
	fail(err)
}

// Component inventory of the installed plugin, skipping like upstream when the claude CLI is absent
func pluginE2EDetails(t *testing.T) string {
	t.Helper()
	pluginE2E.once.Do(installPluginE2E)
	if pluginE2E.unavailable {
		t.Skip("claude CLI not on PATH; install Claude Code to run the plugin E2E suite")
	}
	if pluginE2E.err != nil {
		t.Fatalf("plugin install: %v\n%s", pluginE2E.err, pluginE2E.details)
	}
	return pluginE2E.details
}

func pluginComponentCount(t *testing.T, details, component string) int {
	t.Helper()
	m := regexp.MustCompile(component + `\s+\((\d+)\)`).FindStringSubmatch(details)
	if m == nil {
		t.Fatalf("component inventory has no %q line:\n%s", component+" (N)", details)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func TestCommittedPluginSubtreeLoadsInARealClaudeCodeReportsThePluginAsInstalledRatherThanSilentlyAbsent(t *testing.T) {
	if details := pluginE2EDetails(t); !strings.Contains(details, "Component inventory") {
		t.Fatalf("details\n%s", details)
	}
}

func TestCommittedPluginSubtreeLoadsInARealClaudeCodeParsesAndLoadsTheImpeccableSkill(t *testing.T) {
	details := pluginE2EDetails(t)
	if n := pluginComponentCount(t, details, "Skills"); n != 1 || !regexp.MustCompile(`Skills\s+\(1\)\s+impeccable`).MatchString(details) {
		t.Fatalf("%d skills\n%s", n, details)
	}
}

func TestCommittedPluginSubtreeLoadsInARealClaudeCodeLoadsEveryShippedAgentViaAutoDiscovery(t *testing.T) {
	details := pluginE2EDetails(t)
	shipped := pluginE2E.shipped
	if len(shipped) == 0 {
		t.Fatal("plugin/agents/ ships no agent files")
	}
	if n := pluginComponentCount(t, details, "Agents"); n != len(shipped) {
		t.Fatalf("%d agents loaded, %d shipped\n%s", n, len(shipped), details)
	}
	for _, name := range shipped {
		if !strings.Contains(details, name) {
			t.Errorf("agent %q missing from inventory", name)
		}
	}
}

func TestCommittedPluginSubtreeLoadsInARealClaudeCodeDiscoversThePackagedHooks(t *testing.T) {
	details := pluginE2EDetails(t)
	if n := pluginComponentCount(t, details, "Hooks"); n != 3 {
		t.Fatalf("%d hooks\n%s", n, details)
	}
	wantContains(t, details, "SessionStart", "PostToolUse", "Stop")
}
