package commands

import "testing"

func TestPromptSizeParserMatchesArgparse(t *testing.T) {
	ns, err := bNamespace(t, "prompt-size", []string{"prompt-size"})
	if err != nil || ns["command"] != "prompt-size" || ns["platform"] != "cli" || ns["json"] != false {
		t.Fatalf("defaults %v %v", ns, err)
	}
	ns, err = bNamespace(t, "prompt-size", []string{"prompt-size", "--platform", "telegram", "--json"})
	if err != nil || ns["platform"] != "telegram" || ns["json"] != true {
		t.Fatalf("flags %v %v", ns, err)
	}
	cmd := NewCLI().find("prompt-size")
	if cmd.Short != "Show a byte breakdown of the system prompt + tool schemas" ||
		cmd.Long != "Report the fixed prompt budget for a fresh session: system prompt total, skills index, memory, user profile, and tool-schema JSON. Runs offline (no API call)." {
		t.Fatalf("help %q %q", cmd.Short, cmd.Long)
	}
	if f := cmd.Flags().Lookup("platform"); f == nil || f.Usage != "Platform to simulate (cli, telegram, discord, ...). Default: cli" {
		t.Fatalf("platform flag %+v", f)
	}
	if f := cmd.Flags().Lookup("json"); f == nil || f.Usage != "Emit the breakdown as JSON" {
		t.Fatalf("json flag %+v", f)
	}
}
