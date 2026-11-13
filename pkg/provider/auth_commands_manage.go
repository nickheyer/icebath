package provider

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// The two line free tier rendering shared by every auth display surface
func authFreeTierLines() (string, string) {
	return FreeTierLabel + " · " + GuestModel, UpgradeHint
}

func authIsFreeTierEntry(entry PooledCredential) bool { return isGuestState(entry.Extra) }

// icebath auth list, every pooled provider or the one named, free tier rows rendered as the upgrade hint
func (c *authCLI) list() error {
	providerFilter := c.normalizeProvider(authTextOr(c.ns["provider"]))
	var providers []string
	if providerFilter != "" {
		providers = []string{providerFilter}
	} else {
		data, err := c.env.Store.Load()
		if err != nil {
			return err
		}
		set := map[string]bool{"openrouter": true}
		for _, row := range canonicalRegistryRows(c.ctx) {
			set[row.Name] = true
		}
		for _, p := range ListCustomPoolProviders(c.env.Store) {
			set[p] = true
		}
		for _, entry := range c.customProviderEntries() {
			if key := entry["provider_key"].(string); key != "" {
				set[key] = true
			}
		}
		if pool, ok := data["credential_pool"].(map[string]any); ok {
			for key := range pool {
				set[key] = true
			}
		}
		providers = authSortedKeys(set)
	}
	for _, provider := range providers {
		pool := LoadPool(c.env, provider)
		entries := pool.Entries()
		if len(entries) == 0 {
			continue
		}
		current, hasCurrent := pool.Peek()
		if provider == "nous" && authAllFreeTier(entries) {
			label, hint := authFreeTierLines()
			fmt.Fprintf(c.out, "%s: %s\n", provider, label)
			fmt.Fprintf(c.out, "  %s\n", hint)
			fmt.Fprintln(c.out)
			continue
		}
		fmt.Fprintf(c.out, "%s (%d credentials):\n", provider, len(entries))
		for idx, entry := range entries {
			marker := "  "
			if hasCurrent && entry.ID == current.ID {
				marker = "← "
			}
			row := fmt.Sprintf("  #%d  %-20s %-7s id=%s priority=%d %s%s %s", idx+1, entry.Label, entry.AuthType,
				entry.ID, entry.Priority, authDisplaySource(entry.Source), formatExhaustedStatus(entry), marker)
			fmt.Fprintln(c.out, strings.TrimRight(row, " \t\n\r\v\f"))
		}
		fmt.Fprintln(c.out)
	}
	if providerFilter == "" || ExternalLoginProviders[providerFilter] {
		c.printExternalLoginNotice()
	}
	return nil
}

func authAllFreeTier(entries []PooledCredential) bool {
	for _, entry := range entries {
		if !authIsFreeTierEntry(entry) {
			return false
		}
	}
	return true
}

// One line saying why no Codex CLI or Claude Code login shows up when adoption is off
func (c *authCLI) printExternalLoginNotice() {
	if !AdoptExternalLoginsEnabled(c.env) {
		fmt.Fprintln(c.out, ExternalLoginsNotAdoptedNotice)
	}
	c.printOAuthHealNotices()
}

func (c *authCLI) printOAuthHealNotices() {
	for _, note := range ConsumeOAuthHealNotices() {
		fmt.Fprintf(c.out, "note: %s\n", note)
	}
}

// icebath auth remove, drops the entry then runs its source's removal step and suppression
func (c *authCLI) remove() error {
	provider := c.normalizeProvider(authTextOr(c.ns["provider"]))
	target := c.ns["target"]
	if target == nil {
		target = c.ns["index"]
	}
	pool := LoadPool(c.env, provider)
	index, _, errText := pool.ResolveTarget(authTextOr(target))
	if index == 0 {
		return authExit(fmt.Sprintf("%s Provider: %s.", errText, provider))
	}
	removed, ok := pool.RemoveIndex(index)
	if !ok {
		return authExit(fmt.Sprintf("No credential matching \"%s\" for provider %s.", pyStr(target), provider))
	}
	fmt.Fprintf(c.out, "Removed %s credential #%d (%s)\n", provider, index, removed.Label)
	step, ok := FindRemovalStep(provider, removed.Source)
	if !ok {
		return nil
	}
	result := step.Remove(c.env, provider, removed)
	for _, line := range result.Cleaned {
		fmt.Fprintln(c.out, line)
	}
	if result.Suppress {
		if err := c.env.Store.SuppressCredentialSource(provider, removed.Source); err != nil {
			return err
		}
	}
	for _, line := range result.Hints {
		fmt.Fprintln(c.out, line)
	}
	return nil
}

// icebath auth reset, clears exhaustion on every credential or the one targeted
func (c *authCLI) reset() error {
	provider := c.normalizeProvider(authTextOr(c.ns["provider"]))
	target := c.ns["target"]
	pool := LoadPool(c.env, provider)
	if target == nil || strings.TrimSpace(pyStr(target)) == "" {
		count := pool.ResetStatuses()
		fmt.Fprintf(c.out, "Reset status on %d %s credentials\n", count, provider)
		return nil
	}
	index, matched, errText := pool.ResolveTarget(pyStr(target))
	if index == 0 {
		return authExit(fmt.Sprintf("%s Provider: %s.", errText, provider))
	}
	cleared, ok := pool.ResetStatus(matched.ID)
	if !ok {
		return authExit(fmt.Sprintf("No credential matching \"%s\" for provider %s.", pyStr(target), provider))
	}
	fmt.Fprintf(c.out, "Reset status on %s credential #%d (%s)\n", provider, index, cleared.Label)
	return nil
}

// icebath auth refresh, forces one pooled OAuth entry to rotate its tokens and clear its cooldown
func (c *authCLI) refresh() error {
	provider := c.normalizeProvider(authTextOr(c.ns["provider"]))
	if owned, err := dispatchPluginAuth("refresh", c.pluginArgs(), provider); err != nil || owned {
		return err
	}
	target := c.ns["target"]
	pool := LoadPool(c.env, provider)
	entries := pool.Entries()
	if len(entries) == 0 {
		return authExit(fmt.Sprintf("No %s credentials in the pool.", provider))
	}
	var index int
	var matched PooledCredential
	if target == nil || strings.TrimSpace(pyStr(target)) == "" {
		if len(entries) != 1 {
			return authExit(fmt.Sprintf("%s has %d credentials; pass an index, entry id, or exact "+
				"label (see `icebath auth list %s`).", provider, len(entries), provider))
		}
		index, matched = 1, entries[0]
	} else {
		var errText string
		index, matched, errText = pool.ResolveTarget(pyStr(target))
		if index == 0 {
			return authExit(fmt.Sprintf("%s Provider: %s.", errText, provider))
		}
	}
	if !IsRefreshableOAuthProvider(provider) || matched.AuthType != AuthTypeOAuth || matched.RefreshToken == "" {
		return authExit(fmt.Sprintf("%s credential #%d (%s) is not a refreshable OAuth credential.", provider, index, matched.Label))
	}
	if provider == "nous" && matched.Source != "device_code" {
		return authExit(fmt.Sprintf("nous credential #%d (%s) is not a refreshable OAuth "+
			"credential: only the device_code singleton supports refresh. "+
			"Reauthenticate with `icebath auth add nous --type oauth`.", index, matched.Label))
	}
	refreshed, ok := pool.TryRefreshMatching("", matched.ID)
	if !ok {
		stillPooled := false
		for _, e := range pool.Entries() {
			if e.ID == matched.ID {
				stillPooled = true
			}
		}
		label := provider
		if p, ok := registryProfileCtx(c.ctx, provider); ok {
			label = p.Label()
		}
		state := "the saved session is no longer valid"
		if !stillPooled {
			state = "it was removed from the pool"
		}
		return authExit(fmt.Sprintf("Could not renew the %s sign-in for credential #%d (%s); %s. "+
			"Sign in again with `icebath auth add %s --type oauth`.", label, index, matched.Label, state, provider))
	}
	status := refreshed.LastStatus
	if status == "" {
		status = "ok"
	}
	if status == "ok" {
		fmt.Fprintf(c.out, "Refreshed %s credential #%d (%s); status: ok\n", provider, index, refreshed.Label)
		return nil
	}
	fmt.Fprintf(c.out, "Adopted current tokens for %s credential #%d (%s); status still: %s\n", provider, index, refreshed.Label, status)
	return nil
}

// icebath auth status|logout <provider> after that provider left core for its plugin's own command
func (c *authCLI) movedAuthHint(action, provider string) string {
	return MovedCommandHint(c.ctx, "icebath auth", provider, action)
}

// icebath auth status, the provider's login state with its account fields and Nous handoff lines
func (c *authCLI) status() error {
	provider := c.normalizeProvider(authTextOr(c.ns["provider"]))
	if provider == "" {
		return authExit("Provider is required. Example: `icebath auth status nous`.")
	}
	if owned, err := dispatchPluginAuth("status", c.pluginArgs(), provider); err != nil || owned {
		return err
	}
	if moved := c.movedAuthHint("status", provider); moved != "" {
		return authExit(moved)
	}
	if SingleUseRefreshPoolProviders[provider] {
		LoadPool(c.env, provider)
	}
	status := GetAuthStatus(c.env, provider)
	c.printOAuthHealNotices()
	if pyTruthy(status["free_tier"]) {
		label, hint := authFreeTierLines()
		fmt.Fprintf(c.out, "%s: %s\n", provider, label)
		fmt.Fprintf(c.out, "  %s\n", hint)
		return nil
	}
	if !pyTruthy(status["logged_in"]) {
		line := provider + ": logged out"
		if reason := status["error"]; pyTruthy(reason) {
			line += fmt.Sprintf(" (%s)", pyStr(reason))
		}
		fmt.Fprintln(c.out, line)
		if ExternalLoginProviders[provider] {
			c.printExternalLoginNotice()
		}
		return nil
	}
	fmt.Fprintf(c.out, "%s: logged in\n", provider)
	for _, key := range []string{"auth_type", "client_id", "redirect_uri", "scope", "expires_at", "api_base_url"} {
		if value := status[key]; pyTruthy(value) {
			fmt.Fprintf(c.out, "  %s: %s\n", key, pyStr(value))
		}
	}
	if provider == "nous" {
		if _, err := guestHandoffDrain(c.env); err != nil {
			return err
		}
		handoff, err := guestHandoffStatus(c.env)
		if err != nil {
			return err
		}
		for _, line := range guestHandoffStatusLines(handoff) {
			fmt.Fprintf(c.out, "  %s\n", line)
		}
	}
	return nil
}

// icebath auth logout, the built in path keeps the raw provider id while the alias only picks the handler
func (c *authCLI) logout() error {
	raw := authTextOr(c.ns["provider"])
	normalized := c.normalizeProvider(raw)
	if owned, err := dispatchPluginAuth("logout", c.pluginArgs(), normalized); err != nil || owned {
		return err
	}
	if moved := c.movedAuthHint("logout", normalized); moved != "" {
		return authExit(moved)
	}
	return LogoutCommand(c.env, raw, c.out)
}

var authCmdAWSCallerIdentityFn = awsCallerIdentityARN

// ARN of the principal the default AWS credential chain signs as, through STS GetCallerIdentity
func awsCallerIdentityARN(ctx context.Context, region string) (string, error) {
	creds, err := newAWSChainCredentials(awsSessionOptions{}).Retrieve(ctx)
	if err != nil {
		return "", err
	}
	suffix := "amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		suffix = "amazonaws.com.cn"
	}
	body := []byte("Action=GetCallerIdentity&Version=2011-06-15")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://sts.%s.%s/", region, suffix), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	if _, err := sigV4Sign(req, body, creds, "sts", region, time.Now()); err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sts GetCallerIdentity returned HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Arn string `xml:"GetCallerIdentityResult>Arn"`
	}
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}
	if parsed.Arn == "" {
		return "unknown", nil
	}
	return parsed.Arn, nil
}

// Shows the AWS Bedrock credential status, which lives in the SDK chain rather than the pool
func (c *authCLI) printBedrockStatus() {
	if !hasAWSCredentials(true) {
		return
	}
	region := ResolveBedrockRegion()
	fmt.Fprintln(c.out, "bedrock (AWS SDK credential chain):")
	auth := resolveAWSAuthEnvVar()
	if auth == "" {
		auth = "unknown"
	}
	fmt.Fprintf(c.out, "  Auth: %s\n", auth)
	fmt.Fprintf(c.out, "  Region: %s\n", region)
	if arn, err := authCmdAWSCallerIdentityFn(c.ctx, region); err == nil {
		fmt.Fprintf(c.out, "  Identity: %s\n", arn)
	} else {
		fmt.Fprintln(c.out, "  Identity: (could not resolve — boto3 STS call failed)")
	}
	fmt.Fprintln(c.out)
}

func authStrings(v any) []string {
	var out []string
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		for _, item := range t {
			out = append(out, pyStr(item))
		}
	}
	return out
}

// Shows the Azure Foundry Entra ID status when model.provider is azure-foundry with entra_id auth
func (c *authCLI) printAzureEntraStatus() {
	modelCfg, ok := c.loadConfig()["model"].(map[string]any)
	if !ok || strings.ToLower(strings.TrimSpace(authTextOr(modelCfg["provider"]))) != "azure-foundry" ||
		strings.ToLower(strings.TrimSpace(authTextOr(modelCfg["auth_mode"]))) != "entra_id" {
		return
	}
	baseURL := strings.TrimSpace(authTextOr(modelCfg["base_url"]))
	scope := ""
	if entra, ok := modelCfg["entra"].(map[string]any); ok {
		scope = strings.TrimSpace(authTextOr(entra["scope"]))
	}
	if scope == "" {
		scope = ScopeAIAzureDefault
	}
	fmt.Fprintln(c.out, "azure-foundry (Microsoft Entra ID):")
	endpoint := baseURL
	if endpoint == "" {
		endpoint = "(not configured)"
	}
	fmt.Fprintf(c.out, "  Endpoint: %s\n", endpoint)
	fmt.Fprintf(c.out, "  Scope: %s\n", scope)
	info := DescribeActiveEntraCredential(c.ctx, &EntraIdentityConfig{Scope: scope}, "", 10*time.Second)
	envSources := authStrings(info["env_sources"])
	if pyTruthy(info["ok"]) {
		source := "default chain"
		if len(envSources) > 0 {
			source = strings.Join(envSources, ", ")
		}
		fmt.Fprintf(c.out, "  Status: ✓ token acquired (%s)\n", source)
	} else {
		errText := authTextOr(info["error"])
		if errText == "" {
			errText = "credential chain exhausted"
		}
		fmt.Fprintf(c.out, "  Status: ⚠ %s\n", errText)
		if pyTruthy(info["hint"]) {
			fmt.Fprintf(c.out, "  Hint: %s\n", pyStr(info["hint"]))
		}
	}
	fmt.Fprintln(c.out)
}

// icebath auth upgrade, signs the free tier into a Nous account keeping its connectors
func (c *authCLI) upgrade() error {
	code, err := UpgradeGuest(c.env, UpgradeGuestArgs{Timeout: authTimeout(c.ns, 0), NoBrowser: nsFlag(c.ns, "no_browser")}, c.out)
	if err != nil {
		return err
	}
	if code != 0 {
		return &AuthExitError{Code: code}
	}
	return nil
}

var authStrategyOrder = []string{StrategyFillFirst, StrategyRoundRobin, StrategyLeastUsed, StrategyRandom}

var authStrategyDescriptions = map[string]string{
	StrategyFillFirst:  "Use first key until exhausted, then next",
	StrategyRoundRobin: "Cycle through keys evenly",
	StrategyLeastUsed:  "Always pick the least-used key",
	StrategyRandom:     "Random selection"}

// Writes credential_pool_strategies.<provider> into config.yaml
func (c *authCLI) saveStrategy(provider, strategy string) error {
	h := envHome(c.env)
	cfg := c.loadConfig()
	strategies, ok := cfg["credential_pool_strategies"].(map[string]any)
	if !ok {
		strategies = map[string]any{}
	}
	strategies[provider] = strategy
	cfg["credential_pool_strategies"] = strategies
	return config.SaveConfig(c.ctx, h, cfg, config.SaveOptions{})
}
