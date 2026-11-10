package provider

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Providers select_provider_and_model sends to the generic api key flow
var genericAPIKeyProviders = map[string]bool{
	"openai-api": true, "gemini": true, "deepseek": true, "xai": true, "zai": true, "kimi-coding-cn": true,
	"minimax": true, "minimax-cn": true, "kilocode": true, "opencode-zen": true, "opencode-go": true,
	"alibaba": true, "huggingface": true, "xiaomi": true, "arcee": true, "gmi": true,
	"nvidia": true, "ollama-cloud": true, "tencent-tokenhub": true, "tencent-tokenplan": true, "lmstudio": true,
}

// Picker seams, the provider menu standing for the main_provider_setup module attribute the hermes tests patch
var (
	auxBuildPickerRowsFn        = BuildAuxPickerRows
	setupPromptProviderChoiceFn = promptProviderChoice
	promptMainReasoningEffortFn = promptMainReasoningEffort
	localModelsAvailableFn      = localModelsAvailable
)

// True when the id maps to a registered profile whose auth type is api_key, false on any lookup failure
func isProfileAPIKeyProvider(ctx context.Context, providerID string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	p := getProviderProfile(ctx, providerID)
	return p != nil && p.AuthType == AuthTypeAPIKey
}

// Python v or empty as the receiver of a str method, an AttributeError for a truthy non str value
func pyOrEmptyStr(v any, method string) (string, error) {
	if !pyTruthy(v) {
		return "", nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("AttributeError: '%s' object has no attribute '%s'", config.PyTypeName(v), method)
}

// Sets a key the way a Python dict assignment does, a new key going last in the mapping order
func pyDictSet(m map[string]any, key string, value any) {
	if _, ok := m[key]; ok {
		m[key] = value
		return
	}
	m[key] = value
	config.AppendMappingKey(m, key)
}

// Values Python iteration yields, list items, string characters and mapping keys in their order
func pyIterValues(v any) ([]any, error) {
	switch t := v.(type) {
	case nil:
		return nil, fmt.Errorf("TypeError: 'NoneType' object is not iterable")
	case []any:
		return t, nil
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out, nil
	case string:
		out := make([]any, 0, utf8.RuneCountInString(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case map[string]any:
		keys := config.MappingKeys(t)
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return out, nil
	}
	return nil, fmt.Errorf("TypeError: '%s' object is not iterable", config.PyTypeName(v))
}

// Clears OPENAI_BASE_URL from the home .env unless the configured provider is custom or unset
func clearStaleOpenAIBaseURL(r *ModelFlowRun) error {
	cfg, err := r.loadConfig()
	if err != nil {
		return err
	}
	provider := ""
	if modelCfg, ok := cfg["model"].(map[string]any); ok {
		raw, err := pyOrEmptyStr(modelCfg["provider"], "strip")
		if err != nil {
			return err
		}
		provider = pystr.Lower(pystr.Strip(raw))
	}
	if provider == "custom" || provider == "" {
		return nil
	}
	staleURL, _, err := config.GetEnvValue(r.ctx(), r.home(), "OPENAI_BASE_URL")
	if err != nil {
		return err
	}
	if staleURL == "" {
		return nil
	}
	if err := config.SaveEnvValue(r.home(), "OPENAI_BASE_URL", ""); err != nil {
		return err
	}
	shown := staleURL
	if utf8.RuneCountInString(staleURL) > 40 {
		shown = string([]rune(staleURL)[:40]) + "..."
	}
	fmt.Fprintf(r.Out, "Cleared stale OPENAI_BASE_URL from .env (was: %s)\n", shown)
	return nil
}

// Built in aux tasks in order then plugin registered ones, a failing plugin listing keeping what it yielded
func allAuxTasks(ctx context.Context) []auxTaskRow {
	tasks := append([]auxTaskRow(nil), auxTasks...)
	func() {
		defer func() { _ = recover() }()
		for _, entry := range pluginAuxiliaryTasksFn(ctx) {
			key, hasKey := entry["key"]
			name, hasName := entry["display_name"]
			desc, hasDesc := entry["description"]
			if !hasKey || !hasName || !hasDesc {
				return
			}
			tasks = append(tasks, auxTaskRow{Key: pyStr(key), Name: pyStr(name), Desc: pyStr(desc)})
		}
	}()
	return tasks
}

// Current aux routing for the task menu, custom with the short URL, auto, or the provider with model and effort
func formatAuxCurrent(taskCfg any) string {
	cfg, ok := taskCfg.(map[string]any)
	if !ok {
		return "auto"
	}
	baseURL := pystr.Strip(pyOrStr(cfg["base_url"]))
	provider := "auto"
	if pyTruthy(cfg["provider"]) {
		provider = pyStr(cfg["provider"])
	}
	if provider = pystr.Strip(provider); provider == "" {
		provider = "auto"
	}
	model := pystr.Strip(pyOrStr(cfg["model"]))
	effort := auxEffortWord(cfg)
	suffix := ""
	if model != "" {
		suffix += " · " + model
	}
	if effort != "" {
		suffix += " · " + effort
	}
	if baseURL != "" {
		return "custom (" + shortURL(baseURL) + ")" + suffix
	}
	if provider == "auto" {
		return "auto" + suffix
	}
	return provider + suffix
}

// Stored reasoning_effort as a display word, empty for the provider default and none for YAML false
func auxEffortWord(taskCfg any) string {
	var raw any
	if cfg, ok := taskCfg.(map[string]any); ok {
		raw = cfg["reasoning_effort"]
	}
	if raw == false {
		return "none"
	}
	return pystr.Lower(pystr.Strip(pyOrStr(raw)))
}

// Top level delegation section in aux task shape, an empty provider meaning inherit the parent
func delegationCfgAsTask(cfg map[string]any) map[string]any {
	d, ok := cfg["delegation"].(map[string]any)
	if !ok {
		d = map[string]any{}
	}
	out := map[string]any{}
	for _, k := range []string{"provider", "model", "base_url", "api_key"} {
		out[k] = pystr.Strip(pyOrStr(d[k]))
	}
	effort, has := d["reasoning_effort"]
	if !has {
		effort = ""
	}
	out["reasoning_effort"] = effort
	return out
}

// Stored routing block of a task, delegation read from its top level section
func auxTaskCfg(cfg map[string]any, task string) map[string]any {
	if task == delegationTaskKey {
		return delegationCfgAsTask(cfg)
	}
	aux, _ := cfg["auxiliary"].(map[string]any)
	if entry, ok := aux[task].(map[string]any); ok {
		return entry
	}
	return map[string]any{}
}

// Routing block shown for a task, a plugin task inheriting another shown with its resolved route
func auxTaskCfgForDisplay(ctx context.Context, cfg map[string]any, task string) map[string]any {
	if resolved, ok := inheritedAuxTaskCfg(ctx, task); ok {
		return resolved
	}
	return auxTaskCfg(cfg, task)
}

// Resolved config of a plugin task registered with inherit_from, false when none or the lookup fails
func inheritedAuxTaskCfg(ctx context.Context, task string) (resolved map[string]any, ok bool) {
	defer func() {
		if recover() != nil {
			resolved, ok = nil, false
		}
	}()
	for _, entry := range pluginAuxiliaryTasksFn(ctx) {
		if key, isStr := entry["key"].(string); isStr && key == task && pyTruthy(entry["inherit_from"]) {
			cfg, err := auxGetAuxiliaryTaskConfig(ctx, task)
			if err != nil {
				return nil, false
			}
			return cfg, true
		}
	}
	return nil, false
}

// Display name of a task key, the delegation row included
func auxTaskDisplayName(ctx context.Context, task string) string {
	if task == delegationTaskKey {
		return delegationTaskName
	}
	for _, row := range allAuxTasks(ctx) {
		if row.Key == task {
			return row.Name
		}
	}
	return task
}

// Persists the four routing fields of a task, delegation storing auto as an empty provider
func saveAuxChoice(r *ModelFlowRun, task, provider, model, baseURL, apiKey string, reasoningEffort *string) error {
	tree, err := r.loadConfig()
	if err != nil {
		return err
	}
	cfg := map[string]any(tree)
	var entry map[string]any
	if task == delegationTaskKey {
		entry = ensureDictSection(cfg, "delegation")
		if provider == "auto" {
			provider = ""
		}
	} else {
		entry = ensureDictSection(ensureDictSection(cfg, "auxiliary"), task)
	}
	pyDictSet(entry, "provider", provider)
	pyDictSet(entry, "model", model)
	pyDictSet(entry, "base_url", baseURL)
	pyDictSet(entry, "api_key", apiKey)
	if reasoningEffort != nil && auxTaskTakesReasoning(task) {
		pyDictSet(entry, "reasoning_effort", *reasoningEffort)
	}
	return config.Save(r.home(), tree)
}

// Whether the task block honours reasoning_effort, true for delegation and plugin tasks
func auxTaskTakesReasoning(task string) bool {
	if task == delegationTaskKey {
		return true
	}
	aux, _ := config.Defaults()["auxiliary"].(map[string]any)
	if block, ok := aux[task].(map[string]any); ok {
		_, has := block["reasoning_effort"]
		return has
	}
	return true
}

// Effort step of an aux task, the empty row inheriting the parent for delegation and the provider default otherwise
func promptAuxReasoningEffort(r *ModelFlowRun, task, current string) (string, bool) {
	label := "Provider default"
	if task == delegationTaskKey {
		label = "Inherit parent"
	}
	return promptReasoningEffortSelection(r.Out, config.ValidReasoningEfforts, current, label)
}

// Python value in a set of None, empty and auto, a TypeError for an unhashable value
func auxProviderIsReset(v any, auto string) (bool, error) {
	switch t := v.(type) {
	case nil:
		return true, nil
	case string:
		return t == "" || t == auto, nil
	case map[string]any, []any:
		return false, fmt.Errorf("TypeError: unhashable type: '%s'", config.PyTypeName(v))
	}
	return false, nil
}

// Resets every built in and plugin aux task and the delegation routing to auto, returning how many changed
func resetAuxToAuto(r *ModelFlowRun) (int, error) {
	defaults, _ := config.Defaults()["auxiliary"].(map[string]any)
	clearEntry := func(entry map[string]any, auto, task string) (bool, error) {
		changed := false
		reset, err := auxProviderIsReset(entry["provider"], auto)
		if err != nil {
			return false, err
		}
		if !reset {
			pyDictSet(entry, "provider", auto)
			changed = true
		}
		for _, field := range []string{"model", "base_url", "api_key", "reasoning_effort"} {
			def := ""
			if block, ok := defaults[task].(map[string]any); ok {
				def = pyOrStr(block[field])
			}
			cur := entry[field]
			s, isStr := cur.(string)
			differs := !(isStr && s == def)
			if (pyTruthy(cur) || cur == false || def != "") && differs {
				pyDictSet(entry, field, def)
				changed = true
			}
		}
		return changed, nil
	}
	tree, err := r.loadConfig()
	if err != nil {
		return 0, err
	}
	cfg := map[string]any(tree)
	aux := ensureDictSection(cfg, "auxiliary")
	count := 0
	for _, task := range allAuxTasks(r.ctx()) {
		changed, err := clearEntry(ensureDictSection(aux, task.Key), "auto", task.Key)
		if err != nil {
			return 0, err
		}
		if changed {
			count++
		}
	}
	if dele, ok := cfg["delegation"].(map[string]any); ok {
		changed, err := clearEntry(dele, "", "")
		if err != nil {
			return 0, err
		}
		if changed {
			count++
		}
	}
	if err := config.Save(r.home(), tree); err != nil {
		return 0, err
	}
	return count, nil
}

// Top level auxiliary model picker, looping until Back or a cancel
func auxConfigMenu(r *ModelFlowRun) error {
	for {
		tree, err := r.loadConfig()
		if err != nil {
			return err
		}
		cfg := map[string]any(tree)
		flowSay(r.Out, "", "  Auxiliary models — side-task routing", "",
			"  Side tasks (vision, compression, web extraction, etc.) default",
			`  to your main chat model.  "auto" means "use my main model" —`,
			"  Hermes only falls back to a lightweight backend (OpenRouter,",
			"  Nous Portal) if the main model is unavailable.  Override a",
			"  task below if you want it pinned to a specific provider/model.", "")
		menuTasks := append(allAuxTasks(r.ctx()), auxTaskRow{Key: delegationTaskKey, Name: delegationTaskName, Desc: delegationTaskDesc})
		nameCol, descCol := 0, 0
		for _, t := range menuTasks {
			nameCol = max(nameCol, utf8.RuneCountInString(t.Name))
			descCol = max(descCol, utf8.RuneCountInString(t.Desc))
		}
		nameCol += 2
		descCol += 4
		keys := make([]string, 0, len(menuTasks)+2)
		labels := make([]string, 0, len(menuTasks)+2)
		for _, t := range menuTasks {
			keys = append(keys, t.Key)
			labels = append(labels, pystr.Ljust(t.Name, nameCol)+pystr.Ljust("("+t.Desc+")", descCol)+
				formatAuxCurrent(auxTaskCfgForDisplay(r.ctx(), cfg, t.Key)))
		}
		keys = append(keys, "__reset__", "__back__")
		labels = append(labels, "Reset all to auto", "Back")
		idx, ok := setupPromptProviderChoiceFn(r.Out, labels, 0, "Select provider:")
		if !ok {
			return nil
		}
		switch keys[idx] {
		case "__back__":
			return nil
		case "__reset__":
			n, err := resetAuxToAuto(r)
			if err != nil {
				return err
			}
			msg := "All auxiliary tasks were already set to auto."
			if n != 0 {
				msg = fmt.Sprintf("Reset %d auxiliary task(s) to auto.", n)
			}
			flowSay(r.Out, msg, "")
			continue
		}
		if err := auxSelectForTask(r, keys[idx]); err != nil {
			return err
		}
	}
}

// Aux picker rows of the configured providers, a failure reported as an error like the hermes except
func auxPickerRows(r *ModelFlowRun, currentProvider, currentModel, currentBaseURL string) (rows []map[string]any, err error) {
	defer func() {
		if p := recover(); p != nil {
			rows, err = nil, fmt.Errorf("%v", p)
		}
	}()
	return auxBuildPickerRowsFn(r.ctx(), currentProvider, currentModel, currentBaseURL, nil)
}

// Picks a provider and model for one aux task from the configured providers and persists it
func auxSelectForTask(r *ModelFlowRun, task string) error {
	tree, err := r.loadConfig()
	if err != nil {
		return err
	}
	cfg := map[string]any(tree)
	taskCfg := auxTaskCfg(cfg, task)
	currentProvider := "auto"
	if pyTruthy(taskCfg["provider"]) {
		currentProvider = pyStr(taskCfg["provider"])
	}
	if currentProvider = pystr.Strip(currentProvider); currentProvider == "" {
		currentProvider = "auto"
	}
	currentModel := pystr.Strip(pyOrStr(taskCfg["model"]))
	currentBaseURL := pystr.Strip(pyOrStr(taskCfg["base_url"]))
	displayName := auxTaskDisplayName(r.ctx(), task)
	providers, perr := auxPickerRows(r, currentProvider, currentModel, currentBaseURL)
	if perr != nil {
		fmt.Fprintf(r.Out, "Could not detect authenticated providers: %s\n", perr)
		providers = nil
	}
	autoMarker := ""
	if currentProvider == "auto" && currentBaseURL == "" {
		autoMarker = "  ← current"
	}
	autoLabel := "auto (recommended)"
	if task == delegationTaskKey {
		autoLabel = "auto (inherit main agent)"
	}
	entries := []AuxPickerEntry{{Slug: "__auto__", Label: autoLabel + autoMarker}}
	entries = append(entries, FormatAuxPickerEntries(providers, currentProvider, currentBaseURL)...)
	customMarker := ""
	if currentBaseURL != "" {
		customMarker = "  ← current"
	}
	entries = append(entries, AuxPickerEntry{Slug: "__custom__", Label: "Custom endpoint (direct URL)" + customMarker},
		AuxPickerEntry{Slug: "__back__", Label: "Back"})
	flowSay(r.Out, "", fmt.Sprintf("  Configure %s — current: %s", displayName, formatAuxCurrent(auxTaskCfgForDisplay(r.ctx(), cfg, task))), "")
	labels := make([]string, len(entries))
	for i, e := range entries {
		labels[i] = e.Label
	}
	idx, ok := setupPromptProviderChoiceFn(r.Out, labels, 0, "Select provider:")
	if !ok {
		return nil
	}
	chosen := entries[idx]
	switch chosen.Slug {
	case "__back__":
		return nil
	case "__auto__":
		if err := saveAuxChoice(r, task, "auto", "", "", "", optStr("")); err != nil {
			return err
		}
		fmt.Fprintf(r.Out, "%s: reset to auto.\n", displayName)
		return nil
	case "__custom__":
		return auxFlowCustomEndpoint(r, task, taskCfg)
	}
	return auxFlowProviderModel(r, task, chosen.Slug, chosen.Models, currentModel, auxEffortWord(taskCfg))
}

// Pricing table of a provider for the aux model picker, empty when the lookup fails
func auxPricing(r *ModelFlowRun, providerSlug string) (table PricingTable) {
	defer func() {
		if recover() != nil {
			table = PricingTable{}
		}
	}()
	if table = flowPricingFn(r.ctx(), providerSlug, "", false, false); table == nil {
		table = PricingTable{}
	}
	return table
}

// Model pick under an authenticated provider then its reasoning effort, saved to the aux task
func auxFlowProviderModel(r *ModelFlowRun, task, providerSlug string, curatedModels []string, currentModel, currentEffort string) error {
	displayName := auxTaskDisplayName(r.ctx(), task)
	pricing := auxPricing(r, providerSlug)
	modelList := append([]string{}, curatedModels...)
	var selected string
	if len(modelList) == 0 {
		flowSay(r.Out, fmt.Sprintf("No curated model list for %s.", providerSlug), "Enter a model slug manually (blank = use provider default):")
		typed, ok := flowAskFn(r.Out, "Model: ", flowAskOptions{CancelMsg: optStr("")})
		if !ok {
			return nil
		}
		selected = typed
	} else {
		picked, ok := flowPromptModelSelectionFn(r.Env, r.Out, ModelSelectionPrompt{ModelIDs: modelList, CurrentModel: currentModel,
			Pricing: pricing, ConfirmProvider: providerSlug})
		if !ok {
			fmt.Fprintln(r.Out, flowNoChange)
			return nil
		}
		selected = picked
	}
	var effort *string
	if auxTaskTakesReasoning(task) {
		if e, ok := promptAuxReasoningEffort(r, task, currentEffort); ok {
			effort = &e
		}
	}
	if err := saveAuxChoice(r, task, providerSlug, selected, "", "", effort); err != nil {
		return err
	}
	effortNote := ""
	if effort != nil && *effort != "" {
		effortNote = " · reasoning " + *effort
	}
	if selected != "" {
		fmt.Fprintf(r.Out, "%s: %s · %s%s\n", displayName, providerSlug, selected, effortNote)
	} else {
		fmt.Fprintf(r.Out, "%s: %s (provider default model)%s\n", displayName, providerSlug, effortNote)
	}
	return nil
}

// Direct OpenAI compatible base URL with an optional key and model for one aux task
func auxFlowCustomEndpoint(r *ModelFlowRun, task string, taskCfg map[string]any) error {
	displayName := auxTaskDisplayName(r.ctx(), task)
	currentBaseURL := pystr.Strip(pyOrStr(taskCfg["base_url"]))
	currentModel := pystr.Strip(pyOrStr(taskCfg["model"]))
	flowSay(r.Out, "", "  Custom endpoint for "+displayName,
		"  Provide an OpenAI-compatible base URL (e.g. http://localhost:11434/v1)", "")
	urlPrompt := "Base URL: "
	if currentBaseURL != "" {
		urlPrompt = fmt.Sprintf("Base URL [%s]: ", currentBaseURL)
	}
	baseURL, ok := flowAskFn(r.Out, urlPrompt, flowAskOptions{CancelMsg: optStr("")})
	if !ok {
		return nil
	}
	if baseURL == "" {
		baseURL = currentBaseURL
	}
	if baseURL == "" {
		fmt.Fprintln(r.Out, "No URL provided. No change.")
		return nil
	}
	modelPrompt := "Model slug (optional): "
	if currentModel != "" {
		modelPrompt = fmt.Sprintf("Model slug (optional) [%s]: ", currentModel)
	}
	model, ok := flowAskFn(r.Out, modelPrompt, flowAskOptions{CancelMsg: optStr("")})
	if !ok {
		return nil
	}
	if model == "" {
		model = currentModel
	}
	apiKey, ok := flowAskFn(r.Out, "API key (optional, blank = use OPENAI_API_KEY): ", flowAskOptions{Secret: true, CancelMsg: optStr("")})
	if !ok {
		return nil
	}
	var effort *string
	if auxTaskTakesReasoning(task) {
		if e, ok := promptAuxReasoningEffort(r, task, auxEffortWord(taskCfg)); ok {
			effort = &e
		}
	}
	if err := saveAuxChoice(r, task, "custom", model, baseURL, apiKey, effort); err != nil {
		return err
	}
	line := fmt.Sprintf("%s: custom (%s)", displayName, shortURL(baseURL))
	if model != "" {
		line += " · " + model
	}
	if effort != nil && *effort != "" {
		line += " · reasoning " + *effort
	}
	fmt.Fprintln(r.Out, line)
	return nil
}

// Python value of a dict key with a default for a missing key
func dictGet(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Removes a saved custom provider the user picks from config.yaml
func removeCustomProvider(r *ModelFlowRun) error {
	tree, err := r.loadConfig()
	if err != nil {
		return err
	}
	cfg := map[string]any(tree)
	raw := cfg["custom_providers"]
	providers, isList := raw.([]any)
	if !pyTruthy(raw) || !isList || len(providers) == 0 {
		fmt.Fprintln(r.Out, "No custom providers configured.")
		return nil
	}
	fmt.Fprint(r.Out, "Remove a custom provider:\n\n")
	choices := make([]string, 0, len(providers)+1)
	for _, entry := range providers {
		if m, ok := entry.(map[string]any); ok {
			choices = append(choices, fmt.Sprintf("%s (%s)", pyStr(dictGet(m, "name", "unnamed")), shortURL(pyStr(dictGet(m, "base_url", "")))))
		} else {
			choices = append(choices, pyStr(entry))
		}
	}
	choices = append(choices, "Cancel")
	var idx *big.Int
	if picked, ok := flowRadiolist("Select provider to remove:", plainPickerRows(choices), 0, PickerRadiolistOptions{}); ok {
		fmt.Fprintln(r.Out)
		if picked >= 0 {
			idx = big.NewInt(int64(picked))
		}
	} else {
		for i, c := range choices {
			fmt.Fprintf(r.Out, "  %d. %s\n", i+1, c)
		}
		fmt.Fprintln(r.Out)
		if val, rerr := flowRawInputFn(r.Out, fmt.Sprintf("Choice [1-%d]: ", len(choices))); rerr == nil {
			if val = pystr.Strip(val); val != "" {
				if n, ok := numberedIndex(val); ok {
					idx = n
				}
			}
		}
	}
	if idx == nil || idx.Cmp(big.NewInt(int64(len(providers)))) >= 0 {
		fmt.Fprintln(r.Out, flowNoChange)
		return nil
	}
	pos := int(idx.Int64())
	if idx.Sign() < 0 {
		if idx.Cmp(big.NewInt(int64(-len(providers)))) < 0 {
			return fmt.Errorf("IndexError: pop index out of range")
		}
		pos = len(providers) + pos
	}
	removed := providers[pos]
	rest := append(append([]any{}, providers[:pos]...), providers[pos+1:]...)
	cfg["custom_providers"] = rest
	if err := config.Save(r.home(), tree); err != nil {
		return err
	}
	removedName := pyStr(removed)
	if m, ok := removed.(map[string]any); ok {
		removedName = pyStr(dictGet(m, "name", "unnamed"))
	}
	fmt.Fprintf(r.Out, "✅ Removed \"%s\" from custom providers.\n", removedName)
	return nil
}

// Reasoning effort pick, a level, none, the empty provider default row, or false to keep the current value
func promptReasoningEffortSelection(out io.Writer, efforts []string, currentEffort, defaultLabel string) (string, bool) {
	deduped := []string{}
	seen := map[string]bool{}
	for _, effort := range efforts {
		stripped := pystr.Strip(effort)
		if stripped == "" {
			continue
		}
		v := pystr.Lower(stripped)
		if !seen[v] {
			seen[v] = true
			deduped = append(deduped, v)
		}
	}
	canonicalOrder := []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
	canonical := map[string]bool{}
	ordered := []string{}
	for _, effort := range canonicalOrder {
		canonical[effort] = true
		if seen[effort] {
			ordered = append(ordered, effort)
		}
	}
	for _, effort := range deduped {
		if !canonical[effort] {
			ordered = append(ordered, effort)
		}
	}
	if len(ordered) == 0 {
		return "", false
	}
	indexOf := func(list []string, v string) int {
		for i, s := range list {
			if s == v {
				return i
			}
		}
		return -1
	}
	type tailRow struct {
		value *string
		label string
	}
	tail := []tailRow{{optStr("none"), "Disable reasoning"}}
	if defaultLabel != "" {
		marker := ""
		if currentEffort == "" {
			marker = "  ← currently in use"
		}
		tail = append(tail, tailRow{optStr(""), defaultLabel + marker})
	}
	tail = append(tail, tailRow{nil, "Skip (keep current)"})
	tailIndex := func(v string) int {
		for i, row := range tail {
			if row.value != nil && *row.value == v {
				return i
			}
		}
		return -1
	}
	defaultIdx := 0
	switch {
	case currentEffort == "none":
		defaultIdx = len(ordered) + tailIndex("none")
	case indexOf(ordered, currentEffort) >= 0:
		defaultIdx = indexOf(ordered, currentEffort)
	case defaultLabel != "" && currentEffort == "":
		defaultIdx = len(ordered) + tailIndex("")
	case indexOf(ordered, "medium") >= 0:
		defaultIdx = indexOf(ordered, "medium")
	}
	n := len(ordered)
	rows := make([]string, 0, n+len(tail))
	for _, effort := range ordered {
		if effort == currentEffort {
			rows = append(rows, effort+"  ← currently in use")
		} else {
			rows = append(rows, effort)
		}
	}
	for _, row := range tail {
		rows = append(rows, row.label)
	}
	idx, ok := flowRadiolist("Select reasoning effort:", plainPickerRows(rows), defaultIdx, PickerRadiolistOptions{})
	if ok {
		if idx < 0 {
			return "", false
		}
		fmt.Fprintln(out)
	} else {
		fmt.Fprintln(out, "Select reasoning effort:")
		for i, row := range rows {
			fmt.Fprintf(out, "  %d. %s\n", i+1, row)
		}
		fmt.Fprintln(out)
		picked, blank, cancelled := askIndex(out, fmt.Sprintf("Choice [1-%d] (default: keep current): ", len(rows)), len(rows), false)
		if blank || cancelled {
			return "", false
		}
		idx = picked
	}
	if idx < n {
		return ordered[idx], true
	}
	value := tail[idx-n].value
	if value == nil {
		return "", false
	}
	return *value, true
}

// Offers the effort step after a flow saved a pick or changed model.default, never after a cancel
func offerReasoningAfterPick(r *ModelFlowRun, modelBefore string, savesBefore int) error {
	cfg, err := r.loadConfig()
	if err != nil {
		return err
	}
	modelCfg, ok := cfg["model"].(map[string]any)
	if !ok {
		return nil
	}
	model := pystr.Strip(pyOrStr(modelCfg["default"]))
	if model == "" || (model == modelBefore && ModelChoiceSaveCount() == savesBefore) {
		return nil
	}
	return promptMainReasoningEffortFn(r, model, pyOrStr(modelCfg["provider"]))
}

// Effort step after a main model pick, the supported levels persisted to agent reasoning_effort
func promptMainReasoningEffort(r *ModelFlowRun, model, provider string) error {
	efforts, ok, err := mainModelReasoningEfforts(r.ctx(), model, provider)
	if err != nil || !ok {
		return err
	}
	current, err := r.loadConfig()
	if err != nil {
		return err
	}
	selected, ok := promptReasoningEffortSelection(r.Out, efforts, currentReasoningEffort(current), "")
	if !ok {
		return nil
	}
	cfg, err := r.loadConfig()
	if err != nil {
		return err
	}
	setReasoningEffort(cfg, selected)
	if err := config.Save(r.home(), cfg); err != nil {
		return err
	}
	if selected == "none" {
		fmt.Fprintln(r.Out, "Reasoning disabled for this model.")
	} else {
		fmt.Fprintf(r.Out, "Reasoning effort set to: %s\n", selected)
	}
	return nil
}

// Levels offered for a model on a provider, false when the route has no reasoning control
func mainModelReasoningEfforts(ctx context.Context, model, provider string) ([]string, bool, error) {
	slug := pystr.Lower(pystr.Strip(provider))
	var profile *Profile
	if slug != "" {
		profile = getProviderProfile(ctx, slug)
	}
	if profile != nil && profile.SetupReasoningEfforts != nil {
		if declared := profile.SetupReasoningEfforts(model); declared != nil {
			items, err := pyIterValues(declared)
			if err != nil {
				return nil, false, err
			}
			if len(items) == 0 {
				return nil, false, nil
			}
			out := make([]string, len(items))
			for i, item := range items {
				out[i] = pyStr(item)
			}
			return out, true, nil
		}
	}
	caps, found := safeModelCapabilities(ctx, slug, model)
	if found && caps.SupportsReasoning != nil && !*caps.SupportsReasoning {
		return nil, false, nil
	}
	return append([]string{}, config.ValidReasoningEfforts...), true, nil
}

// models.dev capabilities of a model, not found when the lookup fails
func safeModelCapabilities(ctx context.Context, provider, model string) (caps ModelCapabilities, found bool) {
	defer func() {
		if recover() != nil {
			caps, found = ModelCapabilities{}, false
		}
	}()
	return ModelsDevCapabilities(ctx, provider, model, false)
}
