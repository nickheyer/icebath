package commands

import "testing"

// Port of build_setup_parser in hermes_cli/subcommands/setup.py, the section positional and every flag with its help
func TestCLISetupTreeMatchesBuildSetupParser(t *testing.T) {
	flag := func(name, help string) parserFlag { return parserFlag{name, "", "bool", "false", help} }
	assertParser(t, NewCLI(), "setup", "Interactive setup wizard",
		"Configure Icebath Agent with an interactive wizard. Run a specific section: icebath setup model|tts|terminal|gateway|tools|telemetry|agent",
		"  section   Run a specific setup section instead of the full wizard",
		flag("non-interactive", "Non-interactive mode (use defaults/env vars)"),
		flag("reset", "Reset configuration to defaults"),
		flag("reconfigure", "(Default on existing installs.) Re-run the full wizard, showing current values as defaults. Kept for backwards compatibility — a bare 'icebath setup' now does this."),
		flag("quick", "On existing installs: only prompt for items that are missing or unset, instead of running the full reconfigure wizard."),
		flag("portal", "One-shot Nous Portal setup: log in via OAuth, pick a Nous model, set Nous as the inference provider, and opt into the Tool Gateway. Skips the rest of the wizard."))
}

func TestSetupParserAcceptsTelemetrySection(t *testing.T) {
	ns, err := bNamespace(t, "setup", []string{"setup", "telemetry"})
	if err != nil || ns == nil || ns["section"] != "telemetry" {
		t.Fatalf("namespace %v err %v", ns, err)
	}
	ns, err = bNamespace(t, "setup", []string{"setup", "--reconfigure"})
	if err != nil || ns["reconfigure"] != true || ns["quick"] != false || ns["section"] != nil || ns["non_interactive"] != false || ns["portal"] != false || ns["reset"] != false {
		t.Fatalf("flags namespace %v err %v", ns, err)
	}
	if _, err := bNamespace(t, "setup", []string{"setup", "nope"}); err == nil {
		t.Fatal("a section outside the choices parsed")
	}
}
