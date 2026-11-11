package selfupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Names whose presence in the .env text means a provider credential or endpoint is configured
var doctorProviderEnvHints = []string{
	"DEEPINFRA_API_KEY", "OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN",
	"OPENAI_BASE_URL", "NOUS_API_KEY", "GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY", "KIMI_API_KEY",
	"KIMI_CN_API_KEY", "GMI_API_KEY", "FIREWORKS_API_KEY", "ACTUAL_API_KEY", "ACTUAL_BASE_URL", "MINIMAX_API_KEY",
	"MINIMAX_CN_API_KEY", "KILOCODE_API_KEY", "DEEPSEEK_API_KEY", "DASHSCOPE_API_KEY", "HF_TOKEN",
	"AI_GATEWAY_API_KEY", "OPENCODE_ZEN_API_KEY", "OPENCODE_GO_API_KEY", "COMMANDCODE_API_KEY", "XIAOMI_API_KEY",
	"TOKENHUB_API_KEY", "TOKENPLAN_API_KEY",
}

// Prints the refresh free OAuth status rows of Nous, Codex, MiniMax and xAI
func checkAuthProviders(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		env := doctorPoolEnv(ctx, d.Home)
		doctorLoginRow(d.W, "Nous Portal auth", provider.GetNousAuthStatusLocal(env), "(logged in)", false)
		if !doctorLoginRow(d.W, "OpenAI Codex auth", provider.GetCodexAuthStatus(env), "(logged in)", true) && safeWhich("codex") == "" {
			doctorCheckInfo(d.W, "codex CLI not installed (optional — only required to import tokens from an existing Codex CLI login)")
		}
		minimaxStatus := provider.GetMinimaxOAuthAuthStatus(env)
		region := "global"
		if v, has := minimaxStatus["region"]; has {
			region = pyrepr.Str(v)
		}
		doctorLoginRow(d.W, "MiniMax OAuth", minimaxStatus, "(logged in, region="+region+")", false)
		xaiStatus := provider.GetXAIOAuthAuthStatus(env)
		if !pyrepr.Truthy(xaiStatus) {
			xaiStatus = map[string]any{}
		}
		doctorLoginRow(d.W, "xAI OAuth", xaiStatus, "(logged in)", true)
		return nil
	})
}

// Prints the ok or warn row of an OAuth status, plus its error hint when showError is set
func doctorLoginRow(w io.Writer, label string, status map[string]any, okDetail string, showError bool) bool {
	loggedIn := doctorCheckBool(w, pyrepr.Truthy(status["logged_in"]),
		doctorRow{Text: label, Detail: okDetail}, doctorRow{Text: label, Detail: "(not logged in)"}, false)
	if !loggedIn && showError && pyrepr.Truthy(status["error"]) {
		doctorCheckInfo(w, pyrepr.Str(status["error"]))
	}
	return loggedIn
}

// Persists the dismissal of one advisory id, returning the exit status of the ack only invocation
func doctorAckAdvisory(ctx context.Context, d *Doctor, ackTarget string) int {
	c := &cliui.Console{Out: d.W}
	validIDs := make([]string, 0, len(plugins.Advisories))
	known := false
	for _, a := range plugins.Advisories {
		validIDs = append(validIDs, a.ID)
		known = known || a.ID == ackTarget
	}
	if !known {
		sort.Strings(validIDs)
		ids := strings.Join(validIDs, ", ")
		if ids == "" {
			ids = "(none)"
		}
		fmt.Fprintln(d.W, c.Color("Unknown advisory ID: "+pyrepr.Repr(ackTarget)+". Known IDs: "+ids, cliui.Red))
		return 2
	}
	if plugins.AckAdvisory(ctx, ackTarget) {
		fmt.Fprintln(d.W, c.Color("  ✓ Acknowledged advisory "+ackTarget+". It will no longer trigger startup banners.", cliui.Green))
		return 0
	}
	fmt.Fprintln(d.W, c.Color("  ✗ Could not save the acknowledgement for "+ackTarget+". Make sure "+d.DisplayHome+"/config.yaml is "+
		"writable (`icebath config path` prints the exact file), then re-run "+
		"`icebath doctor --ack "+ackTarget+"`.", cliui.Red))
	return 1
}

// Prints the closing summary, the fixed count, the numbered remaining issues or the all passed line
func doctorPrintSummary(w io.Writer, shouldFix bool, total *Finding) {
	c := &cliui.Console{Out: w}
	fmt.Fprintln(w)
	remaining := append(append([]string{}, total.Issues...), total.ManualIssues...)
	var numbered strings.Builder
	for i, issue := range remaining {
		fmt.Fprintf(&numbered, "  %d. %s\n", i+1, issue)
	}
	rule := strings.Repeat("─", 60)
	switch {
	case shouldFix && total.Fixed > 0:
		fmt.Fprintln(w, c.Color(rule, cliui.Green))
		fmt.Fprint(w, c.Color(fmt.Sprintf("  Fixed %d issue(s).", total.Fixed), cliui.Green, cliui.Bold))
		if len(remaining) > 0 {
			fmt.Fprintln(w, c.Color(fmt.Sprintf(" %d issue(s) require manual intervention.", len(remaining)), cliui.Yellow, cliui.Bold))
		} else {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w)
		if len(remaining) > 0 {
			fmt.Fprintln(w, numbered.String())
		}
	case len(remaining) > 0:
		fmt.Fprintln(w, c.Color(rule, cliui.Yellow))
		fmt.Fprintln(w, c.Color(fmt.Sprintf("  Found %d issue(s) to address:", len(remaining)), cliui.Yellow, cliui.Bold))
		fmt.Fprintln(w)
		fmt.Fprintln(w, numbered.String())
		if !shouldFix && len(total.Issues) > 0 {
			fmt.Fprintln(w, c.Color("  Tip: run 'icebath doctor --fix' to auto-fix what's possible.", cliui.Dim))
		}
	default:
		fmt.Fprintln(w, c.Color(rule, cliui.Green))
		fmt.Fprintln(w, c.Color("  All checks passed! 🎉", cliui.Green, cliui.Bold))
	}
	fmt.Fprintln(w)
}

// Runs the connectivity probes of every configured provider in parallel and prints their rows in order
func checkAPIConnectivity(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		return doctorPrintProbes(ctx, d, f, newDoctorProbeEnv(ctx, d).doctorBuildProbes())
	})
}

// Prints the progress line, runs the probes and prints each result row, collecting the issues that block
func doctorPrintProbes(ctx context.Context, d *Doctor, f *Finding, probes []doctorProbe) error {
	c := &cliui.Console{Out: d.W}
	fmt.Fprint(d.W, "  "+c.Color(fmt.Sprintf("Running %d connectivity checks in parallel…", len(probes)), cliui.Dim))
	results, err := doctorRunProbes(probes)
	if err != nil {
		return err
	}
	fmt.Fprint(d.W, "\r"+strings.Repeat(" ", 70)+"\r")
	for _, r := range results {
		for _, line := range r.Lines {
			row := "  " + line.Glyph + " " + line.Label
			if line.Detail != "" {
				row += " " + line.Detail
			}
			fmt.Fprintln(d.W, row)
		}
		if len(r.Issues) > 0 && !doctorHasHealthyOAuthFallbackForAPIKeyProvider(ctx, d.Home, r.Label) {
			f.Issues = append(f.Issues, r.Issues...)
		}
	}
	return nil
}

// One step of the doctor walk, an optional section title and the check printed under it
type doctorCheckEntry struct {
	Title string
	Check doctorCheckFunc
}

// Ordered doctor walk, an empty title meaning the check prints its own header or none
var doctorChecks = []doctorCheckEntry{
	{"Security Advisories", checkSecurityAdvisories}, {"MCP Server Security", checkMCPSecurity},
	{"Python Environment", checkPythonEnvironment}, {"SSL / CA Certificates", checkCertificates},
	{"Required Packages", checkRequiredPackages}, {"", checkWebDashboardImport},
	{"Configuration Files", checkEnvFile},
	{"", checkConfigFile}, {"", checkConfigDrift},
	{"xAI Model Retirement (May 15, 2026)", checkXAIRetirement},
	{"Session Reset (timers removed Sep 7, 2026)", checkRetiredSessionReset},
	{"Auth Providers", checkAuthProviders},
	{"Directory Structure", checkDirectoryStructure}, {"", checkStateDB}, {"", checkCronStore},
	{"", checkCheckpointStore},
	{"", checkGatewaySupervision}, {"", checkCommandInstallation},
	{"External Tools", checkGitAndRg}, {"", checkTerminalBackend}, {"", checkNodeAndBrowser},
	{"", checkNpmAudit}, {"API Connectivity", checkAPIConnectivity},
	{"Tool Availability", checkToolAvailability}, {"Skills Hub", checkSkillsHub},
	{"Memory Provider", checkMemoryProvider}, {"", checkPluginsDisabledByUpdate},
	{"", checkPluginsInBothLists},
	{"NeMo Relay Plugins", checkRelayPlugins},
	{"", checkProfiles},
}

// Runs every doctor check in order, then the live probes when asked, and returns one when an issue remains
func RunDoctor(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	projectRoot := config.GetProjectRoot()
	if _, err := provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{Home: h, ProjectEnv: filepath.Join(projectRoot, ".env")}); err != nil {
		return 1, err
	}
	return runDoctor(ctx, &Doctor{W: w, Home: h, ProjectRoot: projectRoot, DisplayHome: home.Display(h)}, ns, doctorChecks)
}

// Walks the given checks for one doctor run, or only persists an advisory dismissal when ack is set
func runDoctor(ctx context.Context, d *Doctor, ns map[string]any, checks []doctorCheckEntry) (int, error) {
	w := d.W
	shouldFix := pyrepr.Truthy(ns["fix"])
	if _, set := os.LookupEnv("ICEBATH_INTERACTIVE"); !set {
		os.Setenv("ICEBATH_INTERACTIVE", "1")
	}
	if ack := ns["ack"]; pyrepr.Truthy(ack) {
		return doctorAckAdvisory(ctx, d, pyrepr.Str(ack)), nil
	}
	c := &cliui.Console{Out: w}
	fmt.Fprintln(w)
	for _, line := range []string{
		"┌─────────────────────────────────────────────────────────┐",
		"│                 🩺 Icebath Doctor                       │",
		"└─────────────────────────────────────────────────────────┘",
	} {
		fmt.Fprintln(w, c.Color(line, cliui.Cyan))
	}
	total := &Finding{}
	held := provider.WithLeftCoreInstallsHeld(ctx)
	for _, entry := range checks {
		if entry.Title != "" {
			doctorSection(w, entry.Title)
		}
		found, err := entry.Check(held, d, shouldFix)
		if err != nil {
			return 1, err
		}
		total.Merge(found)
	}
	newDoctorLiveEnv(held, d).doctorMaybeRunLiveChecks(ns, &total.ManualIssues)
	doctorPrintSummary(w, shouldFix, total)
	if len(total.Issues) > 0 || len(total.ManualIssues) > 0 {
		return 1, nil
	}
	return 0, nil
}
