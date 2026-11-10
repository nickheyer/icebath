package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// AgentImportersPlugin is the catalog plugin whose import-agent command brings another agent's setup over
const AgentImportersPlugin = "agent-importers"

// ImportSource is one importer of the agent-importers plugin with the dirs that show the agent is installed
type ImportSource struct {
	Name      string
	Label     string
	Dirs      []string
	ExtraArgs []string
	Exports   []string
}

// ImportSources lists the plugin's importers with the dirs its own auto detection checks
var ImportSources = []ImportSource{
	{Name: "openclaw", Label: "OpenClaw", Dirs: []string{".openclaw", ".clawdbot", ".moltbot"}, ExtraArgs: []string{"--migrate-secrets"}},
	{Name: "claude-code", Label: "Claude Code", Dirs: []string{".claude"}},
	{Name: "codex", Label: "Codex CLI", Dirs: []string{".codex"}},
	{Name: "cursor", Label: "Cursor", Dirs: []string{".cursor"}},
	{Name: "grok-bot", Label: "Grok Bot", Dirs: []string{"Library/Application Support/Grok Bot", "/Applications/Grok Bot.app",
		"Applications/Grok Bot.app", ".config/Grok Bot", "AppData/Roaming/Grok Bot", "AppData/Local/Programs/Grok Bot"},
		Exports: []string{"Downloads/grok-bot-export*"}},
	{Name: "openbot", Label: "OpenBot", Dirs: []string{"Library/Application Support/OpenBot", ".config/OpenBot", "AppData/Roaming/OpenBot", "OpenBot/Agents"}},
}

// IsImportSourceName reports whether name is one of the importers
func IsImportSourceName(name string) bool {
	for _, s := range ImportSources {
		if s.Name == name {
			return true
		}
	}
	return false
}

// FoundImportSource is an importer with the dir or export that showed its agent on this computer
type FoundImportSource struct {
	Source ImportSource
	Home   string
}

// Seams of the import step the tests replace with recorders
var (
	agentImportAsk       = PromptYesNo
	agentImportChecklist = PromptChecklist
	agentImportInstall   = func(ctx context.Context, name string, out io.Writer) error {
		enable := true
		return plugins.CmdInstall(ctx, plugins.InstallCommandOptions{Identifier: name, Enable: &enable, Brief: true}, out)
	}
	agentImportEnable = func(ctx context.Context, name string, out io.Writer) error {
		return plugins.CmdEnable(ctx, name, nil, out)
	}
	agentImportUpdate = func(ctx context.Context, name string, out io.Writer) error {
		return plugins.CmdUpdate(ctx, name, false, false, out)
	}
	agentImportRunChild = runAgentImportChild
	agentImportSelfExe  = os.Executable
	agentImportEnsure   = ensureAgentImportersPlugin
)

// Writer the plugin commands of the import step print on
func agentImportOut() io.Writer { return SetupOutput() }

func userHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// Path.expanduser of a flag value
func expandUserPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		return filepath.Join(userHome(), p[1:])
	}
	return p
}

func agentImportIsDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// DetectImportSources lists the importers whose agent has a setup on this computer, each with the dir found
func DetectImportSources() []FoundImportSource {
	home := userHome()
	var found []FoundImportSource
	for _, source := range ImportSources {
		where := ""
		for _, d := range source.Dirs {
			p := d
			if !filepath.IsAbs(d) {
				p = filepath.Join(home, d)
			}
			if agentImportIsDir(p) {
				where = p
				break
			}
		}
		if where == "" {
			for _, g := range source.Exports {
				matches, _ := filepath.Glob(filepath.Join(home, g))
				sort.Strings(matches)
				if len(matches) > 0 {
					where = matches[0]
					break
				}
			}
		}
		if where != "" {
			found = append(found, FoundImportSource{Source: source, Home: where})
		}
	}
	return found
}

// OfferAgentImport offers to import the agents found on this computer and returns the labels whose import changed config.yaml or .env
func OfferAgentImport(ctx context.Context, icebathHome string, fromSetup bool) (string, error) {
	var found []FoundImportSource
	if !IsNoninteractive() {
		found = DetectImportSources()
	}
	if len(found) == 0 {
		return "", nil
	}
	home := userHome()
	PrintSetupHeader("Coming from another agent?", true)
	where := make([]string, 0, len(found))
	labels := make([]string, 0, len(found))
	for _, f := range found {
		shown := f.Home
		if home != "" && strings.HasPrefix(shown, home) {
			shown = "~" + shown[len(home):]
		}
		where = append(where, fmt.Sprintf("%s (%s)", f.Source.Label, shown))
		labels = append(labels, f.Source.Label)
	}
	contextLines := []string{"Found on this computer: " + strings.Join(where, ", ") + ".",
		"Icebath can bring over its settings, memories, skills and MCP servers.",
		"You'll see a preview and confirm before anything changes."}
	for _, line := range contextLines {
		SetupInfo(line)
	}
	names := "any of them"
	if len(found) <= 2 {
		names = strings.Join(labels, " or ")
	}
	var wrapped []string
	for _, line := range contextLines {
		for _, w := range wrapText(line, 76) {
			wrapped = append(wrapped, "  "+w)
		}
	}
	if !agentImportAsk(fmt.Sprintf("Import your setup from %s?", names), true, strings.Join(wrapped, "\n")) {
		SetupInfo(fmt.Sprintf("Skipped. Import later with: icebath plugins install %s, then icebath import-agent", AgentImportersPlugin))
		return "", nil
	}
	picked := pickImportSources(found)
	if len(picked) == 0 {
		return "", nil
	}
	ok, err := agentImportEnsure(ctx, icebathHome)
	if err != nil || !ok {
		return "", err
	}
	var changed []string
	for _, s := range picked {
		did, err := runAgentImport(ctx, s, icebathHome, fromSetup)
		if err != nil {
			return "", err
		}
		if did {
			changed = append(changed, s.Label)
		}
	}
	return strings.Join(changed, ", "), nil
}

// textwrap.wrap of a line at width on spaces, long words kept whole
func wrapText(text string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// Only one found is that one, several open a checklist with every agent checked
func pickImportSources(found []FoundImportSource) []ImportSource {
	if len(found) == 1 {
		return []ImportSource{found[0].Source}
	}
	labels := make([]string, len(found))
	pre := make([]int, len(found))
	for i, f := range found {
		labels[i], pre[i] = f.Source.Label, i
	}
	picked := agentImportChecklist("Which agents should Icebath import from?", labels, pre)
	if len(picked) == 0 {
		SetupInfo("Nothing selected, so nothing was imported.")
	}
	out := make([]ImportSource, 0, len(picked))
	for _, i := range picked {
		out = append(out, found[i].Source)
	}
	return out
}

// The plugin installed and enabled in the home, installed from the catalog when missing, else enabled
func ensureAgentImportersPlugin(ctx context.Context, icebathHome string) (bool, error) {
	present := PluginPresent(AgentImportersPlugin, icebathHome)
	if !present {
		SetupInfo(nil, fmt.Sprintf("Importing is done by the '%s' plugin from the Icebath plugin catalog. Installing it now...", AgentImportersPlugin))
	}
	var err error
	if present {
		err = agentImportEnable(ctx, AgentImportersPlugin, agentImportOut())
	} else {
		err = agentImportInstall(ctx, AgentImportersPlugin, agentImportOut())
	}
	var exit *plugins.CommandExit
	var refused *plugins.AdmissionRefused
	if err != nil && !errors.As(err, &exit) && !errors.As(err, &refused) {
		return false, err
	}
	if err == nil && PluginPresent(AgentImportersPlugin, icebathHome) {
		return true, nil
	}
	verb, action := "installed", "install"
	if present {
		verb, action = "enabled", "enable"
	}
	SetupPrintWarning(fmt.Sprintf("The '%s' plugin could not be %s, so nothing was imported. Try again later with: icebath plugins %s %s",
		AgentImportersPlugin, verb, action, AgentImportersPlugin))
	return false, nil
}

// Bytes of config.yaml and .env, nil for a missing file
func homeSnapshot(icebathHome string) [2][]byte {
	var out [2][]byte
	for i, name := range []string{"config.yaml", ".env"} {
		if raw, err := os.ReadFile(filepath.Join(icebathHome, name)); err == nil {
			out[i] = raw
		}
	}
	return out
}

func snapshotsEqual(a, b [2][]byte) bool {
	return (a[0] == nil) == (b[0] == nil) && string(a[0]) == string(b[0]) && (a[1] == nil) == (b[1] == nil) && string(a[1]) == string(b[1])
}

// Runs the import child for the source, true when it changed config.yaml or .env
func runAgentImport(ctx context.Context, source ImportSource, icebathHome string, fromSetup bool) (bool, error) {
	before := homeSnapshot(icebathHome)
	if _, err := agentImportChild(ctx, source, icebathHome, nil, fromSetup); err != nil {
		return false, err
	}
	return !snapshotsEqual(homeSnapshot(icebathHome), before), nil
}

// Runs icebath import-agent for the source in a child process, an argparse refusal retried after a plugin update
func agentImportChild(ctx context.Context, source ImportSource, icebathHome string, extraArgs []string, fromSetup bool) (int, error) {
	PrintSetupHeader("Importing from "+source.Label, true)
	resolved, err := pypath.Resolve(icebathHome)
	if err != nil {
		resolved = icebathHome
	}
	var selector []string
	if filepath.Base(filepath.Dir(resolved)) != "profiles" {
		selector = []string{"--profile", "default"}
	}
	exe, err := agentImportSelfExe()
	if err != nil {
		return 0, err
	}
	argv := append([]string{exe}, selector...)
	argv = append(argv, "import-agent", source.Name)
	if fromSetup {
		argv = append(argv, source.ExtraArgs...)
	}
	argv = append(argv, extraArgs...)
	if fromSetup {
		argv = append(argv, "--from-setup")
	}
	env, err := tools.ServedProfileChildEnv(ctx, nil, nil, icebathHome, false)
	if err != nil {
		return 0, err
	}
	returncode, err := agentImportRunChild(argv, env)
	if err != nil {
		return 0, err
	}
	if returncode == 2 && updateAgentImportersPlugin(ctx, source) {
		if returncode, err = agentImportRunChild(argv, env); err != nil {
			return 0, err
		}
	}
	if returncode == 2 {
		SetupPrintWarning(fmt.Sprintf("The %s import did not run. If the error names an invalid choice or unrecognized arguments, update the plugin first: icebath plugins update %s", source.Label, AgentImportersPlugin))
	} else if returncode != 0 {
		SetupPrintWarning(fmt.Sprintf("The %s import stopped with errors; see the report above.", source.Label))
	}
	return returncode, nil
}

// Runs the child on the inherited terminal so the user reads its preview and answers its prompts
func runAgentImportChild(argv []string, env map[string]string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// Brings the installed plugin to the catalog's current pin, false when that update failed
func updateAgentImportersPlugin(ctx context.Context, source ImportSource) bool {
	SetupInfo(nil, fmt.Sprintf("Your '%s' plugin predates the %s importer. Updating it...", AgentImportersPlugin, source.Label))
	err := agentImportUpdate(ctx, AgentImportersPlugin, agentImportOut())
	var exit *plugins.CommandExit
	if errors.As(err, &exit) {
		return false
	}
	return err == nil
}

// MigrateFromAgents runs setup's import step on demand, a named agent installed and imported, bare offering what was found
func MigrateFromAgents(ctx context.Context, icebathHome, sourceName string, extraArgs []string) (int, error) {
	if sourceName == "" {
		found := DetectImportSources()
		if len(found) > 0 && !IsNoninteractive() {
			if _, err := OfferAgentImport(ctx, icebathHome, false); err != nil {
				return 0, err
			}
			return 0, nil
		}
		text := "No other agent setup found on this computer. "
		if len(found) > 0 {
			labels := make([]string, 0, len(found))
			for _, f := range found {
				labels = append(labels, f.Source.Label)
			}
			text = "Found: " + strings.Join(labels, ", ") + ". "
		}
		commands := make([]string, 0, len(ImportSources))
		for _, s := range ImportSources {
			commands = append(commands, "icebath migrate "+s.Name)
		}
		SetupInfo(text + "Import one with: " + strings.Join(commands, ", "))
		return 0, nil
	}
	var source *ImportSource
	for i := range ImportSources {
		if ImportSources[i].Name == sourceName {
			source = &ImportSources[i]
		}
	}
	if source == nil {
		return 0, fmt.Errorf("unknown import source: %s", sourceName)
	}
	ok, err := agentImportEnsure(ctx, icebathHome)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 1, nil
	}
	code, err := agentImportChild(ctx, *source, icebathHome, extraArgs, false)
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 1, nil
	}
	return 0, nil
}
