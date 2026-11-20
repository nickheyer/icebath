package agent

import "testing"

func repairAgent() *AIAgent {
	names := []string{"todo", "patch", "browser_click", "browser_navigate", "web_search", "read_file", "write_file",
		"terminal", "execute_code", "session_search"}
	valid := map[string]bool{}
	for _, n := range names {
		valid[n] = true
	}
	return &AIAgent{validToolNames: valid}
}

func assertRepair(t *testing.T, name, want string) {
	t.Helper()
	got, ok := repairAgent().repairToolCall(name)
	if want == "" {
		if ok {
			t.Fatalf("%q repaired to %q", name, got)
		}
		return
	}
	if !ok || got != want {
		t.Fatalf("%q repaired to %q %v, want %q", name, got, ok, want)
	}
}

func TestExistingBehaviorStillWorksLowercaseAlreadyMatches(t *testing.T) {
	assertRepair(t, "browser_click", "browser_click")
}

func TestClassLikeEmissionsCamelCaseNoSuffix(t *testing.T) {
	assertRepair(t, "BrowserClick", "browser_click")
}

func TestEdgeCasesEmptyString(t *testing.T) {
	assertRepair(t, "", "")
}

func TestVolcEngineXmlPollutionTerminalWithXmlAttributePollution(t *testing.T) {
	assertRepair(t, `terminal" parameter="command" string="true`, "terminal")
}

func TestVolcEngineXmlPollutionToolNameWithTrailingQuoteOnly(t *testing.T) {
	assertRepair(t, `terminal"`, "terminal")
}

func TestVolcEngineXmlPollutionCleanToolNameUnaffectedBySanitizer(t *testing.T) {
	assertRepair(t, "execute_code", "execute_code")
	assertRepair(t, "session_search", "session_search")
}

func TestVolcEngineXmlPollutionSpaceSeparatedNameStillNormalizes(t *testing.T) {
	assertRepair(t, "write file", "write_file")
}

func TestVolcEngineXmlPollutionLeadingQuoteFallsThroughToFuzzyMatch(t *testing.T) {
	assertRepair(t, `"terminal"`, "terminal")
}

func TestRepairStripsDoubledToolSuffixes(t *testing.T) {
	assertRepair(t, "TodoTool_tool", "todo")
	assertRepair(t, "Patch_tool", "patch")
	assertRepair(t, "BrowserClick_tool", "browser_click")
	assertRepair(t, "PatchTool", "patch")
}
