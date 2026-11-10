package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Prompt strings and the config keys the model flows write are behaviour, the key order being the config.yaml key order
const flowNoChange = "No change."

var noteProviderSetupFailureFn = observability.NoteProviderSetupFailure

// Api key prompt of the setup wizard, swappable so tests script the keep replace clear answer
var promptAPIKeyFn = PromptAPIKey

// Post login model picker, swappable so tests script the pick
var flowPromptModelSelectionFn = promptModelSelection

// Provider activation write of the OAuth flows, swappable so tests observe it
var flowUpdateConfigForProviderFn = updateConfigForProvider

// Model choice save of every flow, the auth module attribute the hermes tests patch
var flowSaveModelChoiceFn = saveModelChoice

// Terminal seams the flows read answers through, swappable so tests script the prompts
var (
	flowLineInputFn    = lineInput
	flowRawInputFn     = pickerStdinInput
	flowSecretInputFn  = maskedSecretPrompt
	flowListAgenticFn  = ListAgenticModels
	flowCursesChoiceFn = CursesPromptChoice
	flowRadiolistFn    = cursesRadiolist
)

// Records why a flow returns without a pick for the provider setup metric
func noteSetupFailure(failureClass string) { noteProviderSetupFailureFn(failureClass) }

// Prints each line, an empty string being a blank line, one call per banner block
func flowSay(out io.Writer, lines ...string) { fmt.Fprintln(out, strings.Join(lines, "\n")) }

// flowAskOptions picks the reader of one prompt answer and the message printed on Ctrl-C or EOF
type flowAskOptions struct {
	Secret    bool
	Raw       bool
	CancelMsg *string
	Silent    bool
}

// One stripped prompt answer, false on Ctrl-C or EOF after the cancel message unless silent
func flowAsk(out io.Writer, prompt string, o flowAskOptions) (string, bool) {
	var answer string
	var err error
	switch {
	case o.Secret:
		answer, err = flowSecretInputFn(prompt)
	case o.Raw:
		answer, err = flowRawInputFn(out, prompt)
	default:
		answer, err = flowLineInputFn(prompt)
	}
	if err != nil {
		if !o.Silent {
			msg := "\nCancelled."
			if o.CancelMsg != nil {
				msg = *o.CancelMsg
			}
			fmt.Fprintln(out, msg)
		}
		return "", false
	}
	return strings.TrimSpace(answer), true
}

// Resolves an existing wizard credential and its source without changing its storage
func existingAPIKeyForModelFlow(env PoolEnv, providerID string, pconfig Profile) (string, string) {
	key, source, _ := authResolver(env).apiKeyProviderSecret(providerID, pconfig)
	return key, source
}

// Stored key, the missing hint lines when none exists, then the keep replace clear prompt
func ensureFlowAPIKey(env PoolEnv, out io.Writer, providerID string, pconfig Profile, missingHint []string) (string, string, bool, error) {
	existingKey, existingSource := existingAPIKeyForModelFlow(env, providerID, pconfig)
	if existingKey == "" {
		for _, line := range missingHint {
			fmt.Fprintln(out, line)
		}
	}
	resolved, abort, err := promptAPIKeyFn(env, out, pconfig, existingKey, providerID, existingSource)
	return existingKey, resolved, abort, err
}

// The config and its model section coerced to a map, a bare value becoming default
func loadConfigModelSection(h string) (config.Tree, map[string]any, error) {
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, nil, err
	}
	if cfg == nil {
		cfg = config.Tree{}
	}
	model, ok := cfg["model"].(map[string]any)
	if !ok {
		model = map[string]any{}
		if raw := cfg["model"]; truthy(raw) {
			model["default"] = raw
		}
		pyDictSet(cfg, "model", model)
	}
	return cfg, model, nil
}

// Records the model choice and opens the config model section with provider set
func beginModelConfig(env PoolEnv, selected, providerID string) (config.Tree, map[string]any, error) {
	if err := flowSaveModelChoiceFn(env, selected); err != nil {
		return nil, nil, err
	}
	cfg, model, err := loadConfigModelSection(envHome(env))
	if err != nil {
		return nil, nil, err
	}
	pyDictSet(model, "provider", providerID)
	return cfg, model, nil
}

// Persists the config and deactivates any OAuth provider
func commitModelConfig(env PoolEnv, cfg config.Tree) error {
	h := envHome(env)
	if err := config.Save(h, cfg); err != nil {
		return err
	}
	store := env.Store
	if store == nil {
		store = NewStore(h)
	}
	return store.DeactivateProvider()
}

// persistModelOptions are the endpoint fields written, dropped or kept by the standard persist step
type persistModelOptions struct {
	BaseURL     *string
	APIMode     *string
	DropBaseURL bool
	DropAPIMode bool
	KeepCreds   bool
	Finish      func(cfg config.Tree, model map[string]any)
}

func optStr(s string) *string { return &s }

// Model section with provider, base_url then api_mode, scrubbed credentials, finish, save and deactivate
func persistModel(env PoolEnv, selected, providerID string, o persistModelOptions) (map[string]any, error) {
	cfg, model, err := beginModelConfig(env, selected, providerID)
	if err != nil {
		return nil, err
	}
	if o.BaseURL != nil {
		pyDictSet(model, "base_url", *o.BaseURL)
	}
	if o.APIMode != nil {
		pyDictSet(model, "api_mode", *o.APIMode)
	}
	if o.DropBaseURL {
		delete(model, "base_url")
	}
	if !o.KeepCreds {
		clearModelEndpointCredentials(model, true, o.DropAPIMode, false)
	} else if o.DropAPIMode {
		delete(model, "api_mode")
	}
	if o.Finish != nil {
		o.Finish(cfg, model)
	}
	if err := commitModelConfig(env, cfg); err != nil {
		return nil, err
	}
	return model, nil
}

// Persist plus the confirmation line when selected, else the no change line and nil
func finishModel(env PoolEnv, out io.Writer, selected, providerID, done, noChange string, o persistModelOptions) (map[string]any, error) {
	if selected == "" {
		fmt.Fprintln(out, noChange)
		return nil, nil
	}
	model, err := persistModel(env, selected, providerID, o)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(out, done)
	return model, nil
}

// OAuth provider persist of the model choice and provider config, done or the no change line, nil being silent
func activateProviderModel(env PoolEnv, out io.Writer, selected, providerID, baseURL, done string, noChange *string) error {
	if selected == "" {
		if noChange != nil {
			fmt.Fprintln(out, *noChange)
		}
		return nil
	}
	if err := flowSaveModelChoiceFn(env, selected); err != nil {
		return err
	}
	if _, err := flowUpdateConfigForProviderFn(env, providerID, baseURL, "", false); err != nil {
		return err
	}
	fmt.Fprintln(out, done)
	return nil
}

// The key's section as a map, a missing or non map value replaced
func ensureDictSection(cfg map[string]any, key string) map[string]any {
	section, ok := cfg[key].(map[string]any)
	if !ok {
		section = map[string]any{}
	}
	pyDictSet(cfg, key, section)
	return section
}

// Radio picker when the model list is non empty, else a free text line input, false on cancel
func pickModelOrPrompt(env PoolEnv, out io.Writer, modelList []string, prompt string, p ModelSelectionPrompt) (string, bool) {
	if len(modelList) > 0 {
		p.ModelIDs = modelList
		return flowPromptModelSelectionFn(env, out, p)
	}
	return flowAsk(out, prompt, flowAskOptions{Silent: true})
}

// Retry command and service host for a login helper's failure copy from its provider config
func loginRetryContext(pconfig *Profile) (string, string) {
	if pconfig == nil {
		return "icebath model", "the sign-in service"
	}
	providerID := strings.TrimSpace(pconfig.Name)
	host := "the sign-in service"
	if u, err := url.Parse(strings.TrimSpace(pconfig.PortalBaseURL)); err == nil && u.Hostname() != "" {
		host = strings.ToLower(u.Hostname())
	}
	if providerID == "nous" {
		return "icebath portal", host
	}
	if providerID != "" {
		return "icebath auth add " + providerID, host
	}
	return "icebath model", host
}

// Exit code and message of a login helper's SystemExit, false for any other error
func loginSystemExit(err error) (int, string, bool) {
	var authExit *AuthExitError
	if errors.As(err, &authExit) {
		return authExit.Code, authExit.Message, true
	}
	var oauthExit *OAuthLoginExitError
	if errors.As(err, &oauthExit) {
		return oauthExit.Code, "", true
	}
	var nousExit *NousLoginExitError
	if errors.As(err, &nousExit) {
		return nousExit.Code, "", true
	}
	return 0, "", false
}

// Runs an OAuth login helper, printing plain failure copy with the retry command and returning false on failure
func runLogin(out io.Writer, pconfig *Profile, login func() error) bool {
	retryCommand, serviceHost := loginRetryContext(pconfig)
	err := login()
	if err == nil {
		return true
	}
	if code, msg, ok := loginSystemExit(err); ok {
		switch {
		case msg == "" && (code == 130 || code == 0):
			fmt.Fprintln(out, "Sign-in was cancelled.")
		case strings.TrimSpace(msg) != "":
			fmt.Fprintf(out, "Sign-in did not complete: %s\n", strings.TrimSpace(msg))
			fmt.Fprintf(out, "Run `%s` to try again.\n", retryCommand)
		default:
			fmt.Fprintf(out, "Sign-in did not complete; run `%s` to try again.\n", retryCommand)
		}
		return false
	}
	for _, line := range SignInFailureLines(err, serviceHost, retryCommand) {
		fmt.Fprintln(out, line)
	}
	return false
}

// Login or reuse step for OAuth providers, false when the flow must stop
func oauthGate(out io.Writer, loggedIn bool, name string, pconfig *Profile, login func(forceNewLogin bool) error, freshName string, recheck func() bool) bool {
	if !loggedIn {
		flowSay(out, fmt.Sprintf("Not logged into %s. Starting login...", name), "")
		return runLogin(out, pconfig, func() error { return login(false) })
	}
	flowSay(out, fmt.Sprintf("  %s credentials: ✓", name), "")
	choice := promptAuthCredentialsChoice(out, fmt.Sprintf("%s credentials:", name))
	if choice == "cancel" {
		return false
	}
	if choice == "reauth" {
		if freshName == "" {
			freshName = name
		}
		flowSay(out, fmt.Sprintf("Starting a fresh %s login...", freshName), "")
		if !runLogin(out, pconfig, func() error { return login(true) }) {
			return false
		}
		if recheck != nil && !recheck() {
			fmt.Fprintln(out, "Login failed.")
			return false
		}
	}
	return true
}

// Curated ids then models.dev agentic ids not yet listed, live first providers keeping models.dev order ahead
func modelsDevMerged(ctx context.Context, providerID string, curated []string) []string {
	var mdevModels []string
	func() {
		defer func() { _ = recover() }()
		mdevModels = flowListAgenticFn(ctx, providerID, true)
	}()
	if len(mdevModels) == 0 {
		return []string{}
	}
	primary, secondary := curated, mdevModels
	if liveFirstPickerProviders[providerID] {
		primary, secondary = mdevModels, curated
	}
	canonical := map[string]string{}
	for _, m := range mdevModels {
		canonical[strings.ToLower(m)] = m
	}
	merged := []string{}
	seen := map[string]bool{}
	for _, m := range append(append([]string{}, primary...), secondary...) {
		key := strings.ToLower(m)
		if seen[key] {
			continue
		}
		seen[key] = true
		if c, ok := canonical[key]; ok {
			merged = append(merged, c)
		} else {
			merged = append(merged, m)
		}
	}
	return merged
}

// Tells how many curated models show and how to reach the others
func showCurated(out io.Writer, modelList []string) {
	if len(modelList) > 0 {
		fmt.Fprintf(out, "  Showing %d curated models — use \"Enter custom model name\" for others.\n", len(modelList))
	}
}

// Drops stale model_config entries from inactive custom pools after an explicit custom endpoint switch
func pruneReplacedCustomModelConfigCredentials(env PoolEnv, baseURL, providerName string) {
	defer func() { _ = recover() }()
	activePoolKeys := map[string]bool{}
	for _, key := range customProviderPoolKeyCandidates(baseURL, providerName, env.config()) {
		activePoolKeys[strings.ToLower(strings.TrimSpace(key))] = true
	}
	if len(activePoolKeys) == 0 {
		return
	}
	store := env.Store
	if store == nil {
		store = NewStore(envHome(env))
	}
	pools, err := store.ReadCredentialPools()
	if err != nil || pools == nil {
		return
	}
	for poolKey, entries := range pools {
		if !strings.HasPrefix(poolKey, CustomPoolPrefix) || activePoolKeys[poolKey] || entries == nil {
			continue
		}
		retained := []map[string]any{}
		removedIDs := []string{}
		for _, e := range entries {
			if e != nil && e["source"] == "model_config" {
				if id := e["id"]; truthy(id) {
					removedIDs = append(removedIDs, fmt.Sprint(id))
				}
				continue
			}
			retained = append(retained, e)
		}
		if len(retained) != len(entries) {
			if _, err := store.WriteCredentialPool(poolKey, retained, removedIDs, nil, nil); err != nil {
				return
			}
		}
	}
}

// Full screen single select index, false when curses is unavailable so the caller falls back to a numbered prompt
func cursesChoice(title string, rows []string, defaultIdx int) (int, bool) {
	idx, err := flowCursesChoiceFn(title, rows, defaultIdx, "")
	if err != nil {
		return 0, false
	}
	return idx, true
}

// Plain text rows as unstyled picker rows
func plainPickerRows(rows []string) [][]PickerSegment {
	out := make([][]PickerSegment, len(rows))
	for i, r := range rows {
		out[i] = []PickerSegment{{Text: r}}
	}
	return out
}

// Radio list index with -1 for cancelled, false when curses is unavailable
func flowRadiolist(title string, items [][]PickerSegment, defaultIdx int, opts PickerRadiolistOptions) (int, bool) {
	opts.Selected = defaultIdx
	opts.CancelReturns = -1
	idx, err := flowRadiolistFn(title, items, opts)
	if err != nil {
		return 0, false
	}
	return idx, true
}

// Numbered rows under the title with an arrow at the default
func printNumbered(out io.Writer, title string, rows []string, defaultIdx int) {
	fmt.Fprintln(out, title)
	for i, row := range rows {
		marker := " "
		if i == defaultIdx {
			marker = "→"
		}
		fmt.Fprintf(out, "  %s %d. %s\n", marker, i+1, row)
	}
}

// Use, reauth or cancel from the standard radio prompt with a numbered fallback
func promptAuthCredentialsChoice(out io.Writer, title string) string {
	choices := []string{"Use existing credentials", "Reauthenticate (new OAuth login)", "Cancel"}
	if idx, ok := cursesChoice(title, choices, 0); ok && idx >= 0 {
		fmt.Fprintln(out)
		return []string{"use", "reauth", "cancel"}[idx]
	}
	printNumbered(out, title, choices, 0)
	fmt.Fprintln(out)
	choice, ok := flowAsk(out, "  Choice [1/2/3]: ", flowAskOptions{Raw: true, Silent: true})
	if !ok {
		choice = "1"
	}
	switch choice {
	case "2":
		return "reauth"
	case "3":
		return "cancel"
	}
	return "use"
}
