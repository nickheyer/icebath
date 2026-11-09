package provider

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Interactive credential pool management when icebath auth is called bare
func (c *authCLI) interactiveAuth() error {
	fmt.Fprintln(c.out, "Credential Pool Status")
	fmt.Fprintln(c.out, strings.Repeat("=", 50))
	if err := c.with(map[string]any{"provider": nil}).list(); err != nil {
		return err
	}
	c.printBedrockStatus()
	c.printAzureEntraStatus()
	fmt.Fprintln(c.out)
	choices := []string{"Add a credential", "Remove a credential", "Reset cooldowns for a provider",
		"Set rotation strategy for a provider", "Exit"}
	fmt.Fprintln(c.out, "What would you like to do?")
	for i, choice := range choices {
		fmt.Fprintf(c.out, "  %d. %s\n", i+1, choice)
	}
	raw, _ := c.ask("\nChoice: ", nil)
	switch raw {
	case "1":
		return c.interactiveAdd()
	case "2":
		return c.interactiveRemove()
	case "3":
		return c.interactiveReset()
	case "4":
		return c.interactiveStrategy()
	}
	return nil
}

// Prompts for a provider name with auto complete hints, EOF exiting silently
func (c *authCLI) pickProvider(prompt string) (string, error) {
	unlisted := unlistedProviderNames()
	known := map[string]bool{}
	for _, row := range canonicalRegistryRows(c.ctx) {
		if !unlisted[row.Name] {
			known[row.Name] = true
		}
	}
	known["openrouter"] = true
	var customDisplay []string
	for _, entry := range c.customProviderEntries() {
		customDisplay = append(customDisplay, entry["name"].(string))
	}
	fmt.Fprintf(c.out, "\nKnown providers: %s\n", strings.Join(authSortedKeys(known), ", "))
	if len(customDisplay) > 0 {
		fmt.Fprintf(c.out, "Custom endpoints: %s\n", strings.Join(customDisplay, ", "))
	}
	raw, ok := c.ask(prompt+": ", authCmdLineInputFn)
	if !ok {
		return "", &AuthExitError{Code: 0}
	}
	return c.normalizeProvider(raw), nil
}

func (c *authCLI) interactiveAdd() error {
	provider, err := c.pickProvider("Provider to add credential for")
	if err != nil {
		return err
	}
	if owned, err := dispatchPluginAuth("add", []string{provider}, provider); err != nil || owned {
		return err
	}
	configured := c.configuredProviderEntry(provider)
	if !c.isKnownProvider(provider, configured) {
		return c.unknownProviderExit(provider)
	}
	if err := pluginMissingAuthHandlerError(provider, "add"); err != nil {
		return err
	}
	authType := "api_key"
	if authOAuthCapableProviders[provider] {
		fmt.Fprintf(c.out, "\n%s supports both API keys and OAuth login.\n", provider)
		fmt.Fprintln(c.out, "  1. API key (paste a key from the provider dashboard)")
		fmt.Fprintln(c.out, "  2. OAuth login (authenticate via browser)")
		typeChoice, ok := c.ask("Type [1/2]: ", nil)
		if !ok {
			return nil
		}
		if typeChoice == "2" {
			authType = "oauth"
		}
	}
	label, ok := c.ask("Label / account name (optional): ", authCmdLineInputFn)
	if !ok {
		return nil
	}
	ns := map[string]any{"provider": provider, "auth_type": authType, "label": nil, "api_key": nil,
		"portal_url": nil, "inference_url": nil, "client_id": nil, "scope": nil,
		"no_browser": false, "timeout": nil, "insecure": false, "ca_bundle": nil}
	if label != "" {
		ns["label"] = label
	}
	return c.with(ns).add()
}

func (c *authCLI) interactiveRemove() error {
	provider, err := c.pickProvider("Provider to remove credential from")
	if err != nil {
		return err
	}
	pool := LoadPool(c.env, provider)
	if !pool.HasCredentials() {
		fmt.Fprintf(c.out, "No credentials for %s.\n", provider)
		return nil
	}
	for i, e := range pool.Entries() {
		fmt.Fprintf(c.out, "  #%d  %-25s %-10s %s%s [id:%s]\n", i+1, e.Label, e.AuthType, e.Source, formatExhaustedStatus(e), e.ID)
	}
	raw, _ := c.ask("Remove #, id, or label (blank to cancel): ", authCmdLineInputFn)
	if raw != "" {
		return c.with(map[string]any{"provider": provider, "target": raw}).remove()
	}
	return nil
}

func (c *authCLI) interactiveReset() error {
	provider, err := c.pickProvider("Provider to reset cooldowns for")
	if err != nil {
		return err
	}
	return c.with(map[string]any{"provider": provider}).reset()
}

// Picks a rotation strategy by number, Python list indexing so 0 wraps to the last choice
func (c *authCLI) interactiveStrategy() error {
	provider, err := c.pickProvider("Provider to set strategy for")
	if err != nil {
		return err
	}
	current := GetPoolStrategy(c.env.config(), provider)
	fmt.Fprintf(c.out, "\nCurrent strategy for %s: %s\n", provider, current)
	fmt.Fprintln(c.out)
	for i, s := range authStrategyOrder {
		mark := ""
		if s == current {
			mark = " ←"
		}
		fmt.Fprintf(c.out, "  %d. %-15s — %s%s\n", i+1, s, authStrategyDescriptions[s], mark)
	}
	raw, _ := c.ask("\nStrategy [1-4]: ", nil)
	if raw == "" {
		return nil
	}
	n, err := strconv.Atoi(raw)
	index := n - 1
	if index < 0 {
		index += len(authStrategyOrder)
	}
	if err != nil || index < 0 || index >= len(authStrategyOrder) {
		fmt.Fprintln(c.out, "Invalid choice.")
		return nil
	}
	strategy := authStrategyOrder[index]
	if err := c.saveStrategy(provider, strategy); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Set %s strategy to: %s\n", provider, strategy)
	return nil
}

// Dispatches the auth_action of the namespace, the interactive menu when none was given
func (c *authCLI) run() error {
	switch authTextOr(c.ns["auth_action"]) {
	case "add":
		return c.add()
	case "list":
		return c.list()
	case "remove":
		return c.remove()
	case "reset":
		return c.reset()
	case "priority":
		return c.priority()
	case "refresh":
		return c.refresh()
	case "status":
		return c.status()
	case "logout":
		return c.logout()
	case "upgrade":
		return c.upgrade()
	}
	return c.interactiveAuth()
}

// AuthCommand runs icebath auth from the parsed namespace, SystemExit shaped errors printed to errw with their code
func AuthCommand(ctx context.Context, env PoolEnv, ns map[string]any, w, errw io.Writer) (int, error) {
	cli := &authCLI{ctx: ctx, env: env, ns: ns, out: w, errw: errw}
	err := cli.run()
	if err == nil {
		return 0, nil
	}
	if code, message, ok := authSystemExit(err); ok {
		if message != "" {
			fmt.Fprintln(errw, message)
		}
		return code, nil
	}
	return 1, err
}
