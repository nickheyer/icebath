package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const keptCopilotNotice = "✓ GitHub Copilot moved out of core — installed the 'github-copilot' plugin from the catalog."

// Home bound as the process home with the copilot notice kept by the gateway's start
func keptNoticeHome(t *testing.T) context.Context {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	provider.DeferStartupNotice(ctx, keptCopilotNotice)
	return ctx
}

// Agent of the platform with the per turn guardrails and tool env a loaded agent carries
func keptNoticeAgent(t *testing.T, platform string) *AIAgent {
	t.Helper()
	return &AIAgent{
		platform:         platform,
		apiMode:          "anthropic_messages",
		env:              &tools.Env{Home: t.TempDir(), Values: &sync.Map{}},
		toolGuardrails:   tools.NewToolCallGuardrailController(nil),
		requestOverrides: map[string]any{},
		quietMode:        true,
	}
}

// Loads the tools and runs one per turn reset the way a turn starts
func startAndRunATurn(ctx context.Context, a *AIAgent) *AIAgent {
	loadTools(ctx, a, nil, nil)
	resetPerTurnAgentState(ctx, a)
	return a
}

func TestUnwatchedAgentsLeaveKeptNoticesForTheChatAgent(t *testing.T) {
	ctx := keptNoticeHome(t)
	var notices []provider.AgentNotice
	collect := func(n provider.AgentNotice) { notices = append(notices, n) }
	startAndRunATurn(ctx, keptNoticeAgent(t, "cron"))
	sub := keptNoticeAgent(t, "subagent")
	sub.delegateDepth, sub.noticeCallback = 1, collect
	startAndRunATurn(ctx, sub)
	fork := keptNoticeAgent(t, "telegram")
	fork.persistDisabled, fork.suppressStatusOutput, fork.noticeCallback = true, true, collect
	startAndRunATurn(ctx, fork)
	if len(notices) != 0 {
		t.Fatalf("unwatched agents claimed %v", notices)
	}
	chat := keptNoticeAgent(t, "telegram")
	loadTools(ctx, chat, nil, nil)
	chat.noticeCallback = collect
	resetPerTurnAgentState(ctx, chat)
	resetPerTurnAgentState(ctx, chat)
	if len(notices) != 1 || notices[0].Text != keptCopilotNotice {
		t.Fatalf("chat agent notices %v", notices)
	}
}

func TestACLITurnPrintsWhatAnotherProcessKept(t *testing.T) {
	ctx := keptNoticeHome(t)
	a := keptNoticeAgent(t, "cli")
	var printed []string
	a.printFn = func(text string) { printed = append(printed, text) }
	startAndRunATurn(ctx, a)
	if len(printed) != 1 || printed[0] != keptCopilotNotice {
		t.Fatalf("printed %q", printed)
	}
	if kept := provider.ClaimKeptNotices(ctx); len(kept) != 0 {
		t.Fatalf("still kept %v", kept)
	}
}

func TestARequestRejectedAtTheRowsHostNamesTheInstall(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	acme := provider.LeftCoreFeature{
		Plugin: "acme-llm", Label: "Acme LLM", Unchanged: "your ACME_KEY is unchanged",
		InUse: func(h string, processEnv bool) bool {
			return provider.ProvidersSelected(h, []string{"acme", "acme-acp"}, processEnv)["acme"]
		},
		Providers: []string{"acme", "acme-acp"}, ProfileEnv: []string{"ACME_CLI_PATH"}, Hosts: []string{"acme.test"},
		Notice: func(string, bool) string { return "" },
	}
	t.Cleanup(provider.SwapLeftCoreTableForTests([]provider.LeftCoreFeature{acme}))
	var printed []string
	a := keptNoticeAgent(t, "cli")
	a.printFn = func(text string) { printed = append(printed, text) }
	reject := func(baseURL string) string {
		printed = nil
		err := &rejectedRequestError{"HTTP 400: bad request"}
		status := 400
		turn := &apiErrorTurn{apiError: err, statusCode: &status, providerName: "custom", baseURL: baseURL, model: "m",
			classified: provider.ClassifyError(provider.ClassifyInput{Err: err, Status: 400, Provider: "custom", Model: "m", BaseURL: baseURL})}
		nonretryableClientErrorResult(context.Background(), a, &LoopState{}, turn, "")
		return strings.Join(printed, "\n")
	}
	if out := reject("https://api.eu.acme.test/v1"); !strings.Contains(out, "icebath plugins install acme-llm") {
		t.Fatalf("rows host %q", out)
	}
	if out := reject("https://api.other.test/v1"); strings.Contains(out, "acme-llm") {
		t.Fatalf("other host %q", out)
	}
	provider.RegisterProvider(provider.Profile{Name: "acme", DisplayName: "Acme", AuthType: "api_key"})
	if out := reject("https://api.eu.acme.test/v1"); strings.Contains(out, "acme-llm") {
		t.Fatalf("loaded plugin %q", out)
	}
}

type rejectedRequestError struct{ msg string }

func (e *rejectedRequestError) Error() string { return e.msg }
