package scenario

import (
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// Reports whether the terminal showed the text, wherever the 120 column screen wrapped it
func shownOnTerminal(tm *Term, text string) bool {
	squeeze := func(v string) string { return strings.Join(strings.Fields(v), "") }
	return strings.Contains(squeeze(tm.Text()), squeeze(text))
}

// migrate xai previews, rewrites with a backup and then finds nothing, bare migrate lists the importers
func TestScenarioMigrateXaiAndBareMigrate(t *testing.T) {
	s := Start(t, Setup{Extra: "delegation:\n  model: grok-3\nauxiliary:\n  vision:\n    model: grok-4-fast-non-reasoning\n"})
	before := s.Read("config.yaml")
	dry := s.Run("migrate", "xai").Exit(0)
	dry.Has("xAI Model Retirement Migration", "Found 2 retired xAI model reference(s):",
		"auxiliary.vision.model: 'grok-4-fast-non-reasoning' → use 'grok-4.3'", `(set reasoning_effort: "none")`,
		"delegation.model: 'grok-3' → use 'grok-4.3'", "Migration guide: https://", "Dry-run mode — no changes written.",
		"Re-run with `icebath migrate xai --apply` to rewrite "+s.Path("config.yaml")+" in-place (backup created automatically).")
	if s.Read("config.yaml") != before {
		s.Failf("the dry run rewrote config.yaml")
	}

	applied := s.Run("migrate", "xai", "--apply").Exit(0)
	applied.Has("Found 2 retired xAI model reference(s):", "Backup: ", "Updated 2 slot(s) in "+s.Path("config.yaml"),
		"Run `icebath doctor` to confirm no retired xAI models remain.").Lacks("Dry-run mode")
	backup := strings.TrimSpace(applied.Match(`Backup: (\S+)`)[1])
	if !strings.HasPrefix(backup, s.Home) || !s.Exists(strings.TrimPrefix(backup, s.Home+"/")) {
		s.Failf("the backup %q is not a file of the product home", backup)
	}
	if s.Read(strings.TrimPrefix(backup, s.Home+"/")) != before {
		s.Failf("the backup does not hold the config from before the rewrite")
	}
	s.HasJSON(s.YAML("config.yaml"), `{"delegation": {"model": "grok-4.3"}, "auxiliary": {"vision": {"model": "grok-4.3"}}, "model": {"default": "fake-model"}}`)

	s.Run("migrate", "xai").Exit(0).Has("No retired xAI models in config — nothing to migrate.").Lacks("Found ")
	s.Run("migrate", "xai", "--apply", "--no-backup").Exit(0).Has("No retired xAI models in config — nothing to migrate.").Lacks("Backup: ")

	bare := s.Run("migrate").Exit(0)
	bare.Has("No other agent setup found on this computer. Import one with: icebath migrate openclaw").Lacks("Found: ")
	s.Run("migrate", "no-such-agent").Exit(2).Has("no-such-agent", "icebath migrate")
}

// A second agent's setup in the user home is named by bare migrate when nobody can answer an offer
func TestScenarioMigrateNamesAgentsFoundWithoutATerminal(t *testing.T) {
	s := Start(t, Setup{User: map[string]string{".claude/settings.json": "{}\n"}})
	headless := s.RunStep(Step{Args: []string{"migrate"}, Env: map[string]string{"ICEBATH_NONINTERACTIVE": "1"}}).Exit(0)
	headless.Has("Found: Claude Code. Import one with: icebath migrate openclaw").Lacks("No other agent setup found", "Coming from another agent?")
	named := s.Run("migrate", "claude-code", "--yes").Exit(1)
	named.Has("Importing is done by the 'agent-importers' plugin from the Icebath plugin catalog. Installing it now...",
		"The 'agent-importers' plugin could not be installed, so nothing was imported. Try again later with: icebath plugins install agent-importers")
}

// setup agent asks its three questions on a terminal, saves valid answers and keeps values on bad ones
func TestScenarioSetupAgentSectionOnATerminal(t *testing.T) {
	s := Start(t, Setup{})
	tm := s.Terminal("setup", "agent")
	tm.Await(`Icebath Setup — Agent Settings`)
	tm.Await(`Max iterations \[None\]:`)
	tm.Line("120")
	tm.Await(`Max iterations set to 120`)
	tm.Await(`Tool progress mode \(Enter keeps per-platform defaults\):`)
	tm.Line("Verbose")
	tm.Await(`Tool progress set to: verbose`)
	tm.Await(`Compression threshold \(0\.5-0\.95\) \[0\.5\]:`)
	tm.Line("0.7")
	tm.Await(`Context compression threshold set to 0\.7`)
	tm.Await(`Agent Settings configuration complete!`)
	if code := tm.Wait(); code != 0 {
		s.Failf("setup agent exited %d\n%s", code, clip(tm.Text()))
	}
	s.HasJSON(s.YAML("config.yaml"), `{"agent": {"max_turns": 120, "api_max_retries": 1}, "display": {"tool_progress": "verbose"},
		"compression": {"threshold": 0.7}, "model": {"default": "fake-model"}}`)

	tm = s.Terminal("setup", "agent")
	tm.Await(`Max iterations \[120\]:`)
	tm.Line("many")
	tm.Await(`Invalid number, keeping current value`)
	tm.Await(`Tool progress mode \[verbose\]:`)
	tm.Line("loud")
	tm.Await(`Unknown mode 'loud', keeping 'verbose'`)
	tm.Await(`Compression threshold \(0\.5-0\.95\) \[0\.7\]:`)
	tm.Line("")
	tm.Await(`Context compression threshold set to 0\.7`)
	if code := tm.Wait(); code != 0 {
		s.Failf("setup agent with bad answers exited %d\n%s", code, clip(tm.Text()))
	}
	s.HasJSON(s.YAML("config.yaml"), `{"agent": {"max_turns": 120}, "display": {"tool_progress": "verbose"}, "compression": {"threshold": 0.7}}`)

	s.Run("setup", "agent").Exit(0).Has("Icebath Setup — Non-interactive mode", "Running in a non-interactive environment (no TTY detected).",
		"icebath config set model.provider custom").Lacks("Max iterations")
	s.Run("setup", "no-such-section").Exit(2)
}

// setup telemetry writes one answer to both shared metrics keys and records the consent change
func TestScenarioSetupTelemetrySectionOnATerminal(t *testing.T) {
	s := Start(t, Setup{})
	tm := s.Terminal("setup", "telemetry")
	tm.Await(`Send shared metrics to Nous\?`)
	tm.Await(`→ \(●\) No`)
	tm.Send("\x1b[A")
	tm.Await(`→ \(○\) Yes`)
	tm.Send("\r")
	tm.Await(`Shared metrics sent to Nous once a day\.`)
	tm.Await(`Shared Metrics configuration complete!`)
	if code := tm.Wait(); code != 0 {
		s.Failf("setup telemetry exited %d\n%s", code, clip(tm.Text()))
	}
	if !shownOnTerminal(tm, "☤ Icebath Setup — Shared Metrics") || !shownOnTerminal(tm, "Never prompts, files, paths, setting values or error text.") {
		s.Failf("the section did not print its banner and what shared metrics contain\n%s", clip(tm.Text()))
	}
	s.HasJSON(s.YAML("config.yaml"), `{"telemetry": {"shared_metrics": {"enabled": true, "send": true}}}`)

	tm = s.Terminal("setup", "telemetry")
	tm.Await(`Send shared metrics to Nous\?`)
	tm.Await(`→ \(●\) Yes`)
	tm.Send("\x1b[B")
	tm.Await(`→ \(○\) No`)
	tm.Send("\r")
	tm.Await(`Shared metrics off\.`)
	if code := tm.Wait(); code != 0 {
		s.Failf("setup telemetry answered no exited %d\n%s", code, clip(tm.Text()))
	}
	s.HasJSON(s.YAML("config.yaml"), `{"telemetry": {"shared_metrics": {"enabled": false, "send": false}}}`)
}

// Extra headers of the custom provider entry serving the route reach the model, another route's stay home
func TestScenarioCustomProviderExtraHeadersReachTheModel(t *testing.T) {
	s := Start(t, Setup{
		Model: []any{fakellm.Text{Text: "Headers received."}},
		Extra: "custom_providers:\n  - name: elsewhere\n    base_url: http://127.0.0.1:9/v1\n    extra_headers:\n      X-Scenario-Elsewhere: nope\n" +
			"  - name: scripted\n    base_url: ${MODEL_URL}\n    extra_headers:\n      X-Scenario-Team: blue\n      X-Scenario-Seat: 42\n",
	})
	s.Run("chat", "-q", "Say hello", "-Q").Exit(0).Has("Headers received.")
	var turn *fakellm.Request
	for _, request := range s.Model.Requests() {
		if request.Kind == "main" {
			turn = request
		}
	}
	if turn == nil {
		s.Failf("the model received no turn")
		return
	}
	if turn.Headers["x-scenario-team"] != "blue" || turn.Headers["x-scenario-seat"] != "42" {
		s.Failf("the turn carried headers %v, want x-scenario-team blue and x-scenario-seat 42", turn.Headers)
	}
	if _, sent := turn.Headers["x-scenario-elsewhere"]; sent {
		s.Failf("the header of another route was sent: %v", turn.Headers)
	}
}

// model.options over the gateway socket lists the configured provider row with the current model
func TestScenarioModelOptionsOverTheGatewaySocket(t *testing.T) {
	s := Start(t, Setup{})
	s.Spawn("gateway", "run")
	s.WaitFile("gateway.sock")
	var owner map[string]any
	s.Eventually("the gateway owner reporting ready", 60*time.Second, func() bool {
		owner = controlVerb(s, "identify", nil)
		return owner["state"] == "ready"
	})
	origin, _ := owner["api_origin"].(string)
	grant := controlVerb(s, "session-ticket", map[string]any{"profile_id": s.Home, "instance_id": owner["instance_id"], "purpose": "interactive"})
	ticket, _ := grant["ticket"].(string)
	w := s.Dial("ws"+strings.TrimPrefix(origin, "http")+"/api/ws", "Sec-WebSocket-Protocol", "icebath-gateway-v1, icebath-gateway-ticket."+ticket)
	s.HasJSON(w.Next(), `{"jsonrpc": "2.0", "method": "event", "params": {"type": "gateway.ready"}}`)
	options := w.Call("model.options", map[string]any{})
	s.HasJSON(options, `{"result": {"model": "fake-model", "provider": "custom"}}`)
	rows, _ := Dig(options, "result", "providers").([]any)
	var current map[string]any
	for _, row := range rows {
		if entry, ok := row.(map[string]any); ok && entry["is_current"] == true {
			current = entry
		}
	}
	if current == nil {
		s.Failf("model.options marks no provider row current: %v", options)
		return
	}
	if current["is_user_defined"] != true {
		s.Failf("the current row is not the user defined endpoint: %v", current)
	}
	if _, has := current["usage"]; has {
		s.Failf("a user defined endpoint row carries subscription usage: %v", current)
	}
	if _, has := current["limit"]; has {
		s.Failf("a user defined endpoint row carries a pool limit: %v", current)
	}
}

// Bare migrate on a terminal offers the agent found, and without a reachable catalog imports nothing
func TestScenarioMigrateOffersTheAgentFoundOnATerminal(t *testing.T) {
	s := Start(t, Setup{User: map[string]string{".claude/settings.json": "{}\n"}})
	tm := s.Terminal("migrate")
	tm.Await(`Coming from another agent\?`)
	tm.Await(`Found on this computer: Claude Code \(~/\.claude\)\.`)
	tm.Await(`Import your setup from Claude Code\? \[Y/n\]:`)
	tm.Line("n")
	tm.Await(`Skipped\. Import later with: icebath plugins install agent-importers, then icebath import-agent`)
	if code := tm.Wait(); code != 0 {
		s.Failf("migrate after a declined offer exited %d\n%s", code, clip(tm.Text()))
	}

	tm = s.Terminal("migrate")
	tm.Await(`Import your setup from Claude Code\? \[Y/n\]:`)
	tm.Line("y")
	tm.Await(`Importing is done by the 'agent-importers' plugin`)
	tm.Await(`plugin could not be installed, so nothing was imported`)
	if code := tm.Wait(); code != 0 {
		s.Failf("migrate after a failed plugin install exited %d\n%s", code, clip(tm.Text()))
	}
	for _, line := range []string{"Importing is done by the 'agent-importers' plugin from the Icebath plugin catalog. Installing it now...",
		"The 'agent-importers' plugin could not be installed, so nothing was imported. Try again later with: icebath plugins install agent-importers"} {
		if !shownOnTerminal(tm, line) {
			s.Failf("the terminal did not show %q\n%s", line, clip(tm.Text()))
		}
	}
	if s.Exists("plugins/agent-importers") {
		s.Failf("a failed install left the plugin folder behind")
	}
}

// An installed importer plugin without the agent's command is updated once, then the import is reported as not run
func TestScenarioMigrateReportsAnImporterThatDidNotRun(t *testing.T) {
	s := Start(t, Setup{
		User: map[string]string{".claude/settings.json": "{}\n"},
		Home: map[string]string{"plugins/agent-importers/plugin.yaml": "name: agent-importers\nversion: 0.0.1\ndescription: Scenario fixture without an importer command\n"},
	})
	s.Finding("SF-2")
	before := s.Read("config.yaml")
	tm := s.Terminal("migrate")
	tm.Await(`Import your setup from Claude Code\? \[Y/n\]:`)
	tm.Line("y")
	tm.Await(`Importing from Claude Code`)
	tm.Await(`Your 'agent-importers' plugin predates the Claude Code importer`)
	tm.Await(`The Claude Code import did not run`)
	if code := tm.Wait(); code != 0 {
		s.Failf("migrate after an importer that did not run exited %d\n%s", code, clip(tm.Text()))
	}
	for _, line := range []string{"Your 'agent-importers' plugin predates the Claude Code importer. Updating it...",
		"The Claude Code import did not run. If the error names an invalid choice or unrecognized arguments, update the plugin first: icebath plugins update agent-importers"} {
		if !shownOnTerminal(tm, line) {
			s.Failf("the terminal did not show %q\n%s", line, clip(tm.Text()))
		}
	}
	if strings.Contains(tm.Text(), "Importing is done by the") {
		s.Failf("an installed plugin was installed again\n%s", clip(tm.Text()))
	}
	if !strings.Contains(s.Read("config.yaml"), "fake-model") || strings.Contains(before, "agent-importers") {
		s.Failf("the config lost its model or the fixture began enabled")
	}
}

// icebath model picks a model of the saved endpoint and, a pick having been saved, offers the effort step
func TestScenarioModelCommandOnATerminal(t *testing.T) {
	s := Start(t, Setup{ModelOptions: fakellm.Options{RecordGet: true}, Extra: "custom_providers:\n  - name: scripted\n    base_url: ${MODEL_URL}\n"})
	tm := s.Terminal("model")
	tm.Await(`Select provider:`)
	tm.Await(`→ \(●\) scripted \(127\.0\.0\.1:\d+/v1\)\s+← currently active`)
	tm.Send("\x1b")
	tm.Await(`No change\.`)
	if code := tm.Wait(); code != 0 {
		s.Failf("model after Escape exited %d\n%s", code, clip(tm.Text()))
	}
	if strings.Contains(tm.Text(), "reasoning effort") || strings.Contains(s.Read("config.yaml"), "reasoning_effort") {
		s.Failf("a cancelled pick offered or saved a reasoning effort\n%s", clip(tm.Text()))
	}

	tm = s.Terminal("model")
	tm.Await(`Select provider:`)
	tm.Send("\r")
	tm.Await(`Select model from scripted:`)
	tm.Await(`→ \(●\) fake-model`)
	tm.Send("\r")
	tm.Await(`Select reasoning effort:`)
	tm.Send("\r")
	tm.Await(`Reasoning effort set to: medium`)
	if code := tm.Wait(); code != 0 {
		s.Failf("model after a pick exited %d\n%s", code, clip(tm.Text()))
	}
	for _, line := range []string{"Current model: fake-model", "Active provider: scripted", "Provider: scripted", "URL: ${MODEL_URL}",
		"Found 1 model(s):", "Model set to: fake-model"} {
		if !shownOnTerminal(tm, s.Expand(line)) {
			s.Failf("the terminal did not show %q\n%s", line, clip(tm.Text()))
		}
	}
	s.HasJSON(s.YAML("config.yaml"), `{"model": {"default": "fake-model"}, "agent": {"reasoning_effort": "medium"},
		"custom_providers": [{"name": "scripted", "base_url": "${MODEL_URL}", "model": "fake-model"}]}`)
	listed := false
	for _, request := range s.Model.Requests() {
		listed = listed || (request.Kind == "get" && strings.HasSuffix(request.Path, "/models"))
	}
	if !listed {
		s.Failf("the endpoint was never asked for its models")
	}
}
