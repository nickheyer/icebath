package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/linktier"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	copilotACPMarkerBaseURL = "acp://copilot"
	copilotACPSessionTTL    = 300 * time.Second
	copilotACPFailTTL       = 30 * time.Second
)

// ACP stdio client of the GitHub Copilot CLI with the OpenAI chat facade
type CopilotACPChatClient interface {
	CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error)
	ListModels(ctx context.Context, timeoutSeconds float64) ([]string, error)
	Close() error
}

// Builder of the Copilot CLI ACP client, installed by the acpclient package at init
var NewCopilotACPClient func(ctx context.Context, clientKwargs map[string]any) (CopilotACPChatClient, error)

// GitHub Copilot CLI driven over ACP, an external process with no REST models endpoint
func copilotACPProfile() Profile {
	p := bundledRowProfile(profileRow{name: "copilot-acp", display: "GitHub Copilot ACP",
		desc: "GitHub Copilot ACP (Spawns copilot --acp --stdio)", base: copilotACPMarkerBaseURL, mode: ModeChatCompletions,
		authType: AuthTypeExternalProcess}, "github-copilot-acp", "copilot-acp-agent")
	p.KeyEnv = []string{}
	p.ProcessCommand = "copilot"
	p.ProcessArgs = []string{"--acp", "--stdio"}
	p.ProcessCommandEnvVars = []string{"ICEBATH_COPILOT_ACP_COMMAND", "COPILOT_CLI_PATH"}
	p.ProcessArgsEnvVar = "ICEBATH_COPILOT_ACP_ARGS"
	p.PickerCatalog = copilotACPPickerCatalog
	p.StaticModels = []string{"copilot-acp"}
	p.PickerAfter = "copilot"
	p.PickerGroup = append([]string(nil), copilotPickerGroup...)
	p.CatalogAliases = true
	p.Overlay = &IcebathOverlay{Transport: "codex_responses", AuthType: AuthTypeExternalProcess,
		BaseURLOverride: copilotACPMarkerBaseURL, BaseURLEnvVar: "COPILOT_ACP_BASE_URL"}
	p.OverlayAliases = []string{"github-copilot-acp"}
	p.BaseURLEnv = "COPILOT_ACP_BASE_URL"
	p.RegistryAfter = "copilot"
	p.AccountCard = map[string]string{"name": "GitHub Copilot (ACP)", "flow": "external", "cli_command": "copilot login",
		"docs_url": "https://docs.github.com/en/copilot"}
	p.AccountStatus = copilotACPAccountStatus
	p.AuthHandler = copilotACPAuthHandler
	p.ExternalAuthEvidenceHook = copilotCLIAuthEvidence
	p.CreateClientHook = copilotACPCreateClient
	p.FetchModelsHook = copilotACPFetchModels
	p.LiveContextLengthHook = copilotACPLiveContextLength
	p.InvalidateCatalogCacheHook = clearCopilotACPSessionMemo
	p.NormalizeModelIDHook = copilotACPNormalizeModelID
	return p
}

// Live object view of the ACP client answering chat.completions.create and close
type copilotACPObject struct {
	client CopilotACPChatClient
	depth  int
}

func (o *copilotACPObject) Get(_ context.Context, name string) (any, error) {
	switch {
	case o.depth == 0 && name == "chat":
		return &copilotACPObject{client: o.client, depth: 1}, nil
	case o.depth == 1 && name == "completions":
		return &copilotACPObject{client: o.client, depth: 2}, nil
	}
	return nil, fmt.Errorf("the Copilot ACP client has no %s", name)
}

func (o *copilotACPObject) Call(ctx context.Context, method string, _ []any, kwargs map[string]any) (any, error) {
	switch {
	case o.depth == 2 && method == "create":
		plain, err := hostedJSONMap(kwargs)
		if err != nil {
			return nil, err
		}
		delete(plain, "stream")
		delete(plain, "stream_options")
		return o.client.CreateChatCompletion(ctx, plain)
	case o.depth == 0 && method == "close":
		return nil, o.client.Close()
	}
	return nil, fmt.Errorf("the Copilot ACP client has no %s method", method)
}

// Copilot CLI ACP client from client kwargs, the build line when the plugins tier that carries acpclient is absent
func newCopilotACPChatClient(ctx context.Context, clientKwargs map[string]any) (CopilotACPChatClient, error) {
	if NewCopilotACPClient == nil {
		return nil, errors.New(linktier.Missing("the copilot-acp provider", linktier.Plugins))
	}
	return NewCopilotACPClient(ctx, clientKwargs)
}

// ACP stdio shim client rather than an HTTP client
func copilotACPCreateClient(ctx context.Context, clientKwargs map[string]any) (Client, error) {
	acp, err := newCopilotACPChatClient(ctx, clientKwargs)
	if err != nil {
		return nil, err
	}
	return newHostedSDKClient(copilotACPProfile(), clientKwargs, &copilotACPObject{client: acp}), nil
}

// Launch details of the Copilot CLI or the reason it cannot run
func copilotACPCLIStatus(ctx context.Context) (*Runtime, error) {
	return authResolver(modelsPoolEnv(ctx)).resolveExternalProcessRuntime("copilot-acp", "copilot-acp")
}

// Enabled models a short lived signed in ACP session advertises, false when the CLI is missing or the probe fails
func copilotACPFetchModels(ctx context.Context, _, _ string, timeout time.Duration) (models []string, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			models, ok = nil, false
		}
	}()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	creds, err := copilotACPCLIStatus(ctx)
	if err != nil || !strings.HasPrefix(creds.BaseURL, "acp://") {
		return nil, false
	}
	client, err := newCopilotACPChatClient(ctx, map[string]any{"api_key": creds.APIKey, "base_url": creds.BaseURL,
		"command": creds.Command, "args": creds.Args})
	if err != nil {
		return nil, false
	}
	live, err := client.ListModels(ctx, timeout.Seconds())
	if err != nil || len(live) == 0 {
		return nil, false
	}
	return live, true
}

var (
	copilotACPMemoMu sync.Mutex
	copilotACPMemo   *copilotACPSessionMemo
)

// Memoized session probe, its time, lifetime and models
type copilotACPSessionMemo struct {
	at     time.Time
	ttl    time.Duration
	models []string
}

// Enabled models of a signed in copilot --acp session memoized five minutes, failures thirty seconds
func copilotACPSessionModels(ctx context.Context, forceRefresh bool) []string {
	now := time.Now()
	copilotACPMemoMu.Lock()
	memo := copilotACPMemo
	copilotACPMemoMu.Unlock()
	if !forceRefresh && memo != nil && now.Sub(memo.at) < memo.ttl {
		return memo.models
	}
	var live []string
	func() {
		defer func() {
			if r := recover(); r != nil {
				pkgLog().Debug(fmt.Sprintf("copilot-acp session model discovery failed: %v", r))
				live = nil
			}
		}()
		if p := getProviderProfile(ctx, "copilot-acp"); p != nil {
			if models, ok := p.FetchModels(ctx, "", "", 15*time.Second); ok && len(models) > 0 {
				live = models
			}
		}
	}()
	ttl := copilotACPFailTTL
	if len(live) > 0 {
		ttl = copilotACPSessionTTL
	}
	copilotACPMemoMu.Lock()
	copilotACPMemo = &copilotACPSessionMemo{at: now, ttl: ttl, models: live}
	copilotACPMemoMu.Unlock()
	return live
}

// Drops the session memo so a fresh CLI login shows on the next model switch
func clearCopilotACPSessionMemo() {
	copilotACPMemoMu.Lock()
	copilotACPMemo = nil
	copilotACPMemoMu.Unlock()
}

// Session models of the signed in CLI, else the copilot provider's picker catalog
func copilotACPPickerCatalog(ctx context.Context, forceRefresh bool) ([]string, bool) {
	if live := copilotACPSessionModels(ctx, forceRefresh); len(live) > 0 {
		return live, true
	}
	copilot := getProviderProfile(ctx, "copilot")
	if copilot == nil || copilot.PickerCatalog == nil {
		return nil, false
	}
	return copilot.PickerCatalog(ctx, forceRefresh)
}

// Context window from the same account catalog as the copilot provider
func copilotACPLiveContextLength(ctx context.Context, model, apiKey, baseURL string) int {
	copilot := getProviderProfile(ctx, "copilot")
	if copilot == nil {
		return 0
	}
	return copilot.LiveContextLength(ctx, model, apiKey, baseURL)
}

// Copilot id space of the call's home registry, the ACP marker key never reaching the HTTP catalog
func copilotACPNormalizeModelID(ctx context.Context, model, _ string) string {
	copilot := getProviderProfile(ctx, "copilot")
	if copilot == nil {
		return ""
	}
	return copilot.NormalizeModelID(ctx, model, "")
}

// Accounts card status, signed in only on env token evidence and managed by the CLI otherwise
func copilotACPAccountStatus(ctx context.Context) (any, error) {
	creds, cliErr := copilotACPCLIStatus(ctx)
	verified, authSource, _ := copilotCLIAuthEvidence(ctx)
	configured := cliErr == nil
	var label string
	switch {
	case verified:
		label = authSource
		if label == "" {
			label = "Copilot credentials detected"
		}
	case configured:
		command := creds.Command
		if command == "" {
			command = "copilot"
		}
		label = fmt.Sprintf("Managed by the GitHub Copilot CLI (%s)", command)
	default:
		label = "GitHub Copilot CLI not found on PATH"
	}
	return jsonutil.ObjectOf("logged_in", verified, "source", "copilot_cli", "source_label", label, "token_preview", nil,
		"expires_at", nil, "has_refresh_token", false, "configured", configured), nil
}

// GitHub Copilot ACP model flow through the local Copilot CLI, inside a user started Copilot flow
func modelFlowCopilotACP(r *ModelFlowRun) error {
	return copilotACPModelFlowBody(copilotUserFlowRun(r))
}

func copilotACPModelFlowBody(r *ModelFlowRun) error {
	ctx := r.ctx()
	providerID := "copilot-acp"
	creds, cliErr := copilotACPCLIStatus(ctx)
	resolvedCommand, effectiveBase := "copilot", copilotACPMarkerBaseURL
	if cliErr == nil {
		if creds.Command != "" {
			resolvedCommand = creds.Command
		}
		if creds.BaseURL != "" {
			effectiveBase = creds.BaseURL
		}
	}
	flowSay(r.Out, "  GitHub Copilot ACP delegates Icebath turns to `copilot --acp`.",
		"  Icebath currently starts its own ACP subprocess for each request.",
		"  Icebath uses your selected model as a hint for the Copilot ACP session.",
		"  Command: "+resolvedCommand, "  Backend marker: "+effectiveBase, "")
	if cliErr != nil {
		flowSay(r.Out, "  ⚠ "+cliErr.Error(), "  Set ICEBATH_COPILOT_ACP_COMMAND or COPILOT_CLI_PATH if Copilot CLI is installed elsewhere.")
		return nil
	}
	catalogAPIKey := ""
	if key, _, err := copilotSetupAPIKey(ctx); err == nil {
		catalogAPIKey = key
	}
	_, catalogIDs, normalize := copilotCatalogFor(ctx, catalogAPIKey)
	selected := copilotPickModelOrPrompt(r, copilotModelList(r.Out, catalogIDs), "Model name: ", ModelSelectionPrompt{
		CurrentModel: normalize(r.CurrentModel), ConfirmProvider: providerID, ConfirmBaseURL: effectiveBase, ConfirmAPIKey: catalogAPIKey})
	if selected == "" {
		fmt.Fprintln(r.Out, flowNoChange)
		return nil
	}
	selected = normalize(selected)
	if _, err := persistModel(r.Env, selected, providerID, persistModelOptions{BaseURL: optStr(effectiveBase), APIMode: optStr(string(ModeChatCompletions))}); err != nil {
		return err
	}
	fmt.Fprintf(r.Out, "Default model set to: %s (via GitHub Copilot ACP)\n", selected)
	return nil
}

// icebath auth add copilot-acp runs the GitHub Copilot CLI's own login, the CLI keeping the credential it signs in with
func copilotACPAuthHandler(action string, args []string, _ string) (bool, error) {
	if action != "add" {
		return false, nil
	}
	ctx := context.Background()
	creds, err := copilotACPCLIStatus(ctx)
	if err != nil {
		return false, &PluginAuthError{Message: err.Error() + " GitHub Copilot CLI installs with `npm install -g @github/copilot`; " +
			"then run `icebath auth add copilot-acp` again."}
	}
	command := creds.Command
	if command == "" {
		command = "copilot"
	}
	login := exec.Command(command, "login")
	login.Stdin, login.Stdout, login.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := login.Run(); err != nil {
		return false, &PluginAuthError{Message: fmt.Sprintf("`%s login` did not finish (%v); nothing changed. Run it again to sign GitHub Copilot CLI in.", command, err)}
	}
	clearCopilotACPSessionMemo()
	fmt.Fprintln(os.Stdout, "copilot-acp is signed in through GitHub Copilot CLI.")
	return true, nil
}
