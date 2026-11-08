package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	ackReproUser = "check the current status of the server, grab the latest error logs, " +
		"and let me know if there's anything critical"
	ackReproAck = "I will start by running a health check command on the server to see its current status."
	ackCodeUser = "review the codebase in /app"
	ackCodeAck  = "Let me inspect the repository files first."
)

// ACP agent whose binary is copilot, the only name the on X via acpx cue accepts, unregistered after the test
func ackRegisterACPAgent(t *testing.T) {
	t.Helper()
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	provider.RegisterProvider(provider.Profile{Name: "acme-acp", AuthType: provider.AuthTypeExternalProcess, ProcessCommand: "copilot"})
}

func ackAgent(apiMode string) *AIAgent {
	return &AIAgent{apiMode: apiMode, model: "anthropic/claude-sonnet-4"}
}

func ackUserRows(content any) []*state.Message {
	return []*state.Message{{Role: "user", Content: content}}
}

func TestCodexOnlyPathAcceptsPronounlessWorkspaceAnnouncement(t *testing.T) {
	a := ackAgent("codex_responses")
	user := "add a migration"
	if !a.looksLikeCodexIntermediateAck(user, "Creating the migration file in the repo now.", ackUserRows(user), true) {
		t.Fatal("workspace announcement did not continue")
	}
	if a.looksLikeCodexIntermediateAck(user, "Testing complete. The repo is clean.", ackUserRows(user), true) {
		t.Fatal("a final report continued")
	}
}

func TestMultipartUserMessageDoesNotCrashOnWorkspacePath(t *testing.T) {
	a := ackAgent("codex_responses")
	multipart := []any{
		map[string]any{"type": "text", "text": ackCodeUser},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
	}
	if !a.looksLikeCodexIntermediateAck(multipart, ackCodeAck, ackUserRows(multipart), true) {
		t.Fatal("the text part did not satisfy the workspace requirement")
	}
}

func TestAllPathDropsWorkspaceRequirement(t *testing.T) {
	a := ackAgent("chat_completions")
	if !a.looksLikeCodexIntermediateAck(ackReproUser, ackReproAck, ackUserRows(ackReproUser), false) {
		t.Fatal("opted in intent ack did not continue")
	}
}

func TestPronounlessActionAnnouncementsContinueWhenOptedIn(t *testing.T) {
	ackRegisterACPAgent(t)
	a := ackAgent("chat_completions")
	user := "write the brief and launch the session"
	for _, announcement := range []string{
		"Launching it now.",
		"Launching it on Copilot via acpx.",
		"Writing the task brief, then launching it on Copilot via acpx.",
		"Brief written. Creating the session now.",
		"Brief written. Creating the session on Copilot.",
		"EXIT=1 — checking the actual log.",
		"Exited — checking the actual log.",
		"Relaunching with corrected arguments.",
		"Relaunching with globals before the agent name.",
		"Running now (pid 19441, no early exit). Checking the log…",
		"Brief written\nCreating the session now",
		"Checking whether the service is healthy.",
		"Launching now — see https://ci.example.com/run?id=5 for progress.",
		"Creating the session now. Then launching the worker.",
		"Checking the log now. Will report back.",
		"Exit code 1. Checking the actual log.",
		"Step 2. Creating the session now.",
		"Attempt 2. Relaunching with corrected arguments.",
		"Exit code 1. Attempt 2. Relaunching with corrected arguments.",
		"Exit code 1. Step 2. Checking the actual log.",
		"Attempt 1. Attempt 2. Relaunching with corrected arguments.",
	} {
		if !a.looksLikeCodexIntermediateAck(user, announcement, ackUserRows(user), false) {
			t.Errorf("did not continue: %q", announcement)
		}
	}
}

func TestPronounlessActionGuardrailsRejectQuestionsAndFinals(t *testing.T) {
	ackRegisterACPAgent(t)
	a := ackAgent("chat_completions")
	user := "launch the session"
	for _, final := range []string{
		"Should I launch it now?",
		"Nothing to do here.",
		"Interesting result; no action is needed.",
		"Done. The deployment succeeded.",
		"Testing completed successfully.",
		"Checking finished with no issues.",
		"Testing has completed successfully.",
		"Checking is complete.",
		"Running was successful.",
		"Testing complete.",
		"Checking done.",
		"Running successful.",
		"Testing the parser completed successfully.",
		"Checking the logs finished with no errors.",
		"Testing completed in 3.2 seconds.",
		"Checking finished at 10:42.",
		"Running the suite passed 42 of 42 assertions.",
		"Testing completed and everything looks good.",
		"Running the suite passed. 42 tests, 0 failures.",
		"Running the suite in parallel with pytest-xdist is the fastest win.",
		"Checking the CI logs would be the first step.",
		"Reading the traceback tells you which parser rule failed.",
		"Reading the traceback gives you the failing rule.",
		"Running the suite locally reproduces the failure.",
		"Checking the CI logs seems like the first step.",
		"Running the suite locally reproduced the failure.",
		"Checking the CI logs revealed a stale cache.",
		"Testing the parser found three bugs.",
		"Reviewing the diff surfaced two issues.",
		"Running the tests fails intermittently.",
		"Running the suite that ships with the repo reproduces the failure.",
		"Reading the traceback that pytest prints gives the failing rule.",
		"Testing the branch that CI builds found three bugs.",
		"Reading the traceback explains the failure.",
		"Checking the logs confirms the theory.",
		"Running the migration breaks the schema.",
		"Testing the parser produces a stack trace.",
		"Creating the migration that adds the users table.",
		"Checking the job that failed in CI.",
		"Running the migration via script improves speed.",
		"Running now — this improves reliability.",
		"Checking the actual log reveals the cause.",
		"Relaunching with corrected arguments improves reliability.",
		"Checking whether the service is healthy improves reliability.",
		"Then launching the worker improves reliability.",
		"Running the suite now. 42 passed, 0 failed.",
		"Checking the log now. Testing completed successfully.",
		"Checking the actual log. The job failed because the token expired.",
		"Running the suite locally works now.",
		"Creating the index speeds queries now.",
		"Running the suite works on Copilot.",
		"Testing happens via acpx.",
		"Checking the actual log. The error was a missing token.",
		"Checking the log now. The build is green.",
		"Launching it now. The deployment is live.",
		"Reviewing the diff. Everything looks fine.",
		"Testing the parser.\n\nAll 42 tests pass.",
		"Running the numbers. The total is 42.",
		"A few options:\n- Running the suite with a fixed seed\n- Pinning the dependency.",
		"Two ideas:\n* Checking the CI cache\n* Bumping the timeout.",
		"Options:\n1. Running the suite locally.",
		"Two options. 1. Running the suite locally. 2. Pinning the dependency.",
		"Two options. 1. Pinning the dependency. 2. Running the suite locally.",
		"Two options. 1. Checking the actual log. 2. Bumping the timeout.",
		"Two options. 1. Relaunching with corrected arguments. 2. Pinning the dep.",
		"Two approaches. 1. Checking the actual log. 2. Bumping the timeout.",
		"Two approaches. 1. Pinning the dependency. 2. Checking the actual log.",
		"Two ways. 1. Creating the session now. 2. Pinning the dep.",
		"Two options. Step 1. Pinning the dependency. Step 2. Relaunching with corrected arguments.",
		"Step 1. Pinning the dependency. Step 2. Relaunching with corrected arguments.",
		"Step 1. Pinning the dependency. Step 3. Relaunching with corrected arguments.",
		"Two options. 2. Pinning the dependency. 3. Checking the actual log.",
		"Possible fixes. 1. Checking the actual log. 2. Bumping the timeout.",
		"Two options: pinning the dep, checking the actual log.",
		"Two fixes: bumping the timeout, relaunching with corrected arguments.",
		"Option: 1. Launching it now.",
		"Options: 1. Creating the session now.",
	} {
		if a.looksLikeCodexIntermediateAck(user, final, ackUserRows(user), false) {
			t.Errorf("continued on a final answer: %q", final)
		}
	}
}
