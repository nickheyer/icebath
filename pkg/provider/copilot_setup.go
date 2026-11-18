package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	copilotDeviceExpiresIn = 900
	copilotTokenEnvVar     = "COPILOT_GITHUB_TOKEN"
)

// Device code endpoints of the GitHub web host
var (
	copilotDeviceLoginBase       = "https://github.com"
	copilotDeviceVerificationURL = "https://github.com/login/device"
)

// Terminal device login outcomes with their status and message
var copilotDeviceErrors = map[string][2]string{"expired_token": {"expired", "Device code expired. Please try again."},
	"access_denied": {"denied", "Authorization was denied."}}

// Live Copilot ids, else the curated list with a warning
func copilotModelList(out io.Writer, liveIDs []string) []string {
	if len(liveIDs) > 0 {
		list := []string{}
		for _, id := range liveIDs {
			if id != "" {
				list = append(list, id)
			}
		}
		fmt.Fprintf(out, "  Found %d model(s) from GitHub Copilot\n", len(list))
		return list
	}
	list := append([]string(nil), copilotStaticModels...)
	if len(list) > 0 {
		flowSay(out, "  ⚠ Could not auto-detect models from GitHub Copilot — showing defaults.",
			`    Use "Enter custom model name" if you do not see your model.`)
	}
	return list
}

// Catalog, its ids and a normalizer canonicalizing a model id against it for a GitHub token
func copilotCatalogFor(ctx context.Context, apiKey string) ([]any, []string, func(string) string) {
	catalog := fetchGithubModelCatalog(ctx, apiKey, copilotCatalogTimeout)
	ids := []string{}
	for _, item := range catalog {
		if id := cpGetOr(item, "id", ""); cpTruthy(id) {
			ids = append(ids, pyStr(id))
		}
	}
	normalize := func(mid string) string {
		if n := normalizeCopilotModelID(ctx, mid, catalog, apiKey); n != "" {
			return n
		}
		return mid
	}
	return catalog, ids, normalize
}

// Exchanged Copilot token of the first supported env token with its env name, the raw token when the exchange fails
func copilotSetupAPIKey(ctx context.Context) (string, string, error) {
	raw, source, err := resolveCopilotToken(ctx)
	if err != nil {
		return "", "", err
	}
	if raw == "" {
		return "", "", nil
	}
	token, _ := getCopilotAPIToken(ctx, raw)
	return token, source, nil
}

// Every id the models endpoint lists for the token without chat filtering, empty on any failure
func copilotRawModelIDs(ctx context.Context, apiKey string) []string {
	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	payload, err := copilotGetJSON(ctx, copilotModelsURL, copilotCatalogTimeout, headers)
	if err != nil {
		return []string{}
	}
	ids := []string{}
	for _, item := range copilotPayloadItems(payload) {
		if id := cpGetOr(item, "id", nil); cpTruthy(id) {
			ids = append(ids, pyStr(id))
		}
	}
	return ids
}

// Picker over the model list or a free text answer, empty on cancel
func copilotPickModelOrPrompt(r *ModelFlowRun, modelList []string, prompt string, p ModelSelectionPrompt) string {
	selected, ok := pickModelOrPromptFn(r.Env, r.Out, modelList, prompt, p)
	if !ok {
		return ""
	}
	return selected
}

// Offers the device code login or a pasted token when no Copilot token exists, false to stop
func copilotObtainToken(r *ModelFlowRun) (bool, error) {
	flowSay(r.Out, "No GitHub token configured for GitHub Copilot.", "", "  Supported token types:",
		"    → OAuth token (gho_*/ghu_*)    via the device code flow below",
		"    → Fine-grained PAT (github_pat_*)  with Copilot Requests permission",
		"    → GitHub App token (ghu_*)     via environment variable",
		"    ✗ Classic PAT (ghp_*)          NOT supported by Copilot API", "", "  Options:",
		"    1. Login with GitHub (OAuth device code flow)", "    2. Enter a token manually", "    3. Cancel", "")
	blank := ""
	choice, ok := flowAskFn(r.Out, "  Choice [1-3]: ", flowAskOptions{CancelMsg: &blank})
	if !ok {
		return false, nil
	}
	switch choice {
	case "1":
		token, err := copilotDeviceCodeLogin(r.ctx(), r.Out, "github.com", 300*time.Second)
		if (err != nil || token == "") && r.ctx().Err() != nil {
			fmt.Fprintln(r.Out, "  Sign-in was cancelled.")
			return false, nil
		}
		if err == nil && token == "" {
			fmt.Fprintln(r.Out, "  Login cancelled or failed.")
			return false, nil
		}
		if err == nil {
			err = config.SaveEnvValueContext(r.ctx(), r.home(), copilotTokenEnvVar, token)
		}
		if err != nil {
			fmt.Fprintf(r.Out, "  Login failed: %v\n", err)
			return false, nil
		}
		flowSay(r.Out, "  Copilot token saved.", "")
		return true, nil
	case "2":
		newKey, ok := flowAskFn(r.Out, "  Token (COPILOT_GITHUB_TOKEN): ", flowAskOptions{Secret: true, CancelMsg: &blank})
		if !ok {
			return false, nil
		}
		if newKey == "" {
			fmt.Fprintln(r.Out, "  Cancelled.")
			return false, nil
		}
		if valid, msg := validateCopilotToken(newKey); !valid {
			fmt.Fprintf(r.Out, "  ✗ %s\n", msg)
			return false, nil
		}
		if err := config.SaveEnvValueContext(r.ctx(), r.home(), copilotTokenEnvVar, newKey); err != nil {
			return false, err
		}
		flowSay(r.Out, "  Token saved.", "")
		return true, nil
	}
	fmt.Fprintln(r.Out, "  Cancelled.")
	return false, nil
}

// GitHub Copilot model flow through env tokens or the device code login, inside a user started Copilot flow
func modelFlowCopilot(r *ModelFlowRun) error {
	return copilotModelFlowBody(copilotUserFlowRun(r))
}

func copilotModelFlowBody(r *ModelFlowRun) error {
	ctx := r.ctx()
	providerID := "copilot"
	apiKey, source, err := copilotSetupAPIKey(ctx)
	if err != nil {
		return err
	}
	if apiKey == "" {
		obtained, err := copilotObtainToken(r)
		if err != nil || !obtained {
			return err
		}
		if apiKey, _, err = copilotSetupAPIKey(ctx); err != nil {
			return err
		}
	} else if source == "GITHUB_TOKEN" || source == "GH_TOKEN" {
		flowSay(r.Out, fmt.Sprintf("  GitHub token: %s... ✓ (%s%s)", pystr.Slice(apiKey, 0, 8), source, SecretSourceLabel(r.home(), source)), "")
	} else {
		flowSay(r.Out, "  GitHub token: ✓", "")
	}
	effectiveBase := copilotBaseURL
	catalog, liveModels, normalize := copilotCatalogFor(ctx, apiKey)
	if len(catalog) == 0 {
		liveModels = copilotRawModelIDs(ctx, apiKey)
	}
	selected := copilotPickModelOrPrompt(r, copilotModelList(r.Out, liveModels), "Model name: ", ModelSelectionPrompt{
		CurrentModel: normalize(r.CurrentModel), ConfirmProvider: providerID, ConfirmBaseURL: effectiveBase, ConfirmAPIKey: apiKey})
	if selected == "" {
		fmt.Fprintln(r.Out, flowNoChange)
		return nil
	}
	selected = normalize(selected)
	mode := string(copilotModelAPIMode(ctx, selected, catalog, apiKey))
	if _, err := persistModel(r.Env, selected, providerID, persistModelOptions{BaseURL: optStr(effectiveBase), APIMode: optStr(mode)}); err != nil {
		return err
	}
	fmt.Fprintf(r.Out, "Default model set to: %s (via GitHub Copilot)\n", selected)
	return nil
}

// Per model effort set Copilot publishes, empty when the model takes none
func copilotSetupReasoningEfforts(model string) any {
	return githubModelReasoningEfforts(context.Background(), model, nil, "")
}

// Accounts card status from the env token ladder only, no exchange
func copilotAccountStatus(ctx context.Context) (any, error) {
	h := home.Dir(ctx)
	for _, envVar := range copilotEnvVars {
		raw, _, err := config.GetEnvValue(ctx, h, envVar)
		if err != nil {
			return nil, err
		}
		token := pystr.Strip(raw)
		if valid, _ := validateCopilotToken(token); token != "" && valid {
			return pyjson.ObjectOf("logged_in", true, "source", "env_var", "source_label", envVar+SecretSourceLabel(h, envVar),
				"token_preview", nil, "expires_at", nil, "has_refresh_token", false), nil
		}
	}
	return pyjson.ObjectOf("logged_in", false, "source", nil), nil
}

// Python int of a JSON value, the ValueError or TypeError text when int raises
func copilotPyInt(v any) (int, error) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case float64:
		if math.IsInf(n, 0) || math.IsNaN(n) {
			return 0, fmt.Errorf("cannot convert float %v to integer", n)
		}
		return int(n), nil
	case string:
		i, err := strconv.Atoi(pystr.Strip(strings.ReplaceAll(n, "_", "")))
		if err != nil {
			return 0, fmt.Errorf("%s is not a whole number", pyStringRepr(n))
		}
		return i, nil
	}
	if f, ok := cpFloat(v); ok {
		return int(f), nil
	}
	return 0, fmt.Errorf("expected a whole number, got %s", errchain.Kind(v))
}

// Python int of value or a default when the value is falsy
func copilotPyIntOr(v any, def int) (int, error) {
	if !cpTruthy(v) {
		return def, nil
	}
	return copilotPyInt(v)
}

// Asks GitHub for a device code with the setup OAuth app for the in app login
func copilotStartDeviceLogin(ctx context.Context) (any, error) {
	data, err := copilotPostForm(ctx, copilotDeviceLoginBase+"/login/device/code",
		[][2]string{{"client_id", copilotOAuthClientID}, {"scope", "read:user"}}, 15*time.Second)
	if err != nil {
		return nil, err
	}
	deviceCode, userCode := cpGetOr(data, "device_code", ""), cpGetOr(data, "user_code", "")
	if !cpTruthy(deviceCode) || !cpTruthy(userCode) {
		return nil, errors.New("GitHub did not return a device code.")
	}
	interval, err := copilotPyIntOr(cpGetOr(data, "interval", nil), copilotDeviceCodePollInterval)
	if err != nil {
		return nil, err
	}
	interval = max(interval, 1)
	expiresIn, err := copilotPyIntOr(cpGetOr(data, "expires_in", nil), copilotDeviceExpiresIn)
	if err != nil {
		return nil, err
	}
	verification := cpGetOr(data, "verification_uri", nil)
	if !cpTruthy(verification) {
		verification = copilotDeviceVerificationURL
	}
	return map[string]any{"user_code": userCode, "verification_url": verification, "expires_in": expiresIn,
		"interval": interval + copilotDeviceCodePollSafetyMargin,
		"state":    map[string]any{"device_code": deviceCode, "interval": interval}}, nil
}

// One token poll of the in app login, a transport failure being another pending tick
func copilotPollDeviceLogin(ctx context.Context, state any) (any, error) {
	deviceCode, ok := cpGet(state, "device_code")
	if !ok {
		return nil, errors.New("the Copilot device login state has no device_code")
	}
	result, err := copilotPostForm(ctx, copilotDeviceLoginBase+"/login/oauth/access_token",
		[][2]string{{"client_id", copilotOAuthClientID}, {"device_code", pyStr(deviceCode)},
			{"grant_type", "urn:ietf:params:oauth:grant-type:device_code"}}, 10*time.Second)
	if err != nil {
		var notDict *copilotNotDictError
		if errors.As(err, &notDict) {
			return nil, err
		}
		return map[string]any{"status": "pending"}, nil
	}
	if token := cpGetOr(result, "access_token", nil); cpTruthy(token) {
		return map[string]any{"status": "approved", "credential": token}, nil
	}
	errValue := cpGetOr(result, "error", "")
	errText, _ := errValue.(string)
	if errText == "slow_down" {
		var interval int
		if n, ok := cpPositiveNumber(cpGetOr(result, "interval", nil)); ok {
			interval = int(n)
		} else {
			prev, err := copilotPyIntOr(cpGetOr(state, "interval", nil), 1)
			if err != nil {
				return nil, err
			}
			interval = prev + 5
		}
		next := map[string]any{}
		if m, ok := state.(map[string]any); ok {
			for k, v := range m {
				next[k] = v
			}
		} else {
			for _, k := range state.(*pyjson.Object).Keys {
				next[k] = state.(*pyjson.Object).Vals[k]
			}
		}
		next["interval"] = interval
		return map[string]any{"status": "pending", "interval": interval + copilotDeviceCodePollSafetyMargin, "state": next}, nil
	}
	if !cpTruthy(errValue) || errText == "authorization_pending" {
		return map[string]any{"status": "pending"}, nil
	}
	outcome, known := copilotDeviceErrors[errText]
	if !known {
		outcome = [2]string{"error", "Authorization failed: " + pyStr(errValue)}
	}
	return map[string]any{"status": outcome[0], "message": outcome[1]}, nil
}

// Saves the approved GitHub OAuth token where the terminal device login puts it
func copilotSaveDeviceLogin(ctx context.Context, token any) error {
	return config.SaveEnvValueContext(ctx, home.Dir(ctx), copilotTokenEnvVar, pyStr(token))
}

// Value following a flag in the raw auth arguments, empty when the flag is absent
func copilotAuthArg(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// Both ends of the terminal, so a piped run never blocks on the device poll
func copilotHasTerminal() bool { return streamIsTTY(os.Stdin) && streamIsTTY(os.Stdout) }

// icebath auth add copilot signs in by GitHub device code and pools the token, other actions stay with core
func copilotAuthHandler(action string, args []string, _ string) (bool, error) {
	if action != "add" || copilotAuthArg(args, "--api-key") != "" {
		return false, nil
	}
	authType := strings.ToLower(pystr.Strip(copilotAuthArg(args, "--type")))
	if authType == "api-key" || authType == "api_key" || !copilotHasTerminal() {
		return false, nil
	}
	ctx := context.Background()
	token, err := copilotDeviceCodeLogin(ctx, os.Stdout, "github.com", 300*time.Second)
	if err != nil {
		return false, err
	}
	if token == "" {
		return false, &PluginAuthError{Message: "GitHub sign-in did not complete; nothing was saved."}
	}
	if valid, msg := validateCopilotToken(token); !valid {
		return false, &PluginAuthError{Message: msg}
	}
	pool := LoadPool(modelsPoolEnv(ctx), "copilot")
	label := pystr.Strip(copilotAuthArg(args, "--label"))
	if label == "" {
		label = fmt.Sprintf("github-device-%d", len(pool.Entries())+1)
	}
	if _, err := pool.AddEntry(PooledCredential{Provider: "copilot", ID: authNewEntryID(), Label: label, AuthType: AuthTypeAPIKey,
		Priority: 0, Source: "manual:device_code", AccessToken: token, BaseURL: copilotBaseURL}); err != nil {
		return false, err
	}
	fmt.Fprintf(os.Stdout, "Added copilot credential #%d: \"%s\"\n", len(pool.Entries()), label)
	return true, nil
}
