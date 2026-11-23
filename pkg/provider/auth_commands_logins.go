package provider

import (
	"fmt"
	"io"
	"strings"
)

// One listed login row of icebath auth list with its default, rotation and in use markers
func authLoginRow(index int, l Login) string {
	var markers []string
	if l.Default {
		markers = append(markers, "default")
	}
	if l.Rotate {
		markers = append(markers, "rotate")
	}
	source := authDisplaySource(l.Source)
	if strings.HasPrefix(l.Source, SourceManual) {
		if source == SourceManual {
			source = "icebath"
		} else {
			source = "icebath:" + source
		}
	}
	account := l.Account
	if account == "" {
		account = "-"
	}
	row := fmt.Sprintf("  #%d  %-20s %-7s %-6s %-26s %-28s id=%s priority=%d", index, l.Name, l.AuthType, l.Origin, source, account, l.ID, l.Priority)
	if len(markers) > 0 {
		row += " [" + strings.Join(markers, ",") + "]"
	}
	if l.Status != LoginStatusReady {
		row += " " + strings.ReplaceAll(l.Status, "_", " ")
		if l.ResetsAt > 0 {
			row += " until " + epochTime(l.ResetsAt).Local().Format("15:04")
		}
	}
	if l.Label != "" && l.Label != l.Name {
		row += fmt.Sprintf(" label=%q", l.Label)
	}
	if l.SharedWith != "" {
		row += " (also used by " + l.SharedWith + ")"
	}
	if l.Active {
		row += " ← in use"
	}
	return strings.TrimRight(row, " ")
}

// Header line of one provider's logins with its default and strategy
func authLoginHeader(group LoginGroup) string {
	def := group.Default
	if def == "" {
		def = "none (every login in order)"
	} else if def != LoginRotateSelection {
		for _, l := range group.Logins {
			if l.ID == def {
				def = l.Name
				break
			}
		}
	}
	noun := "logins"
	if len(group.Logins) == 1 {
		noun = "login"
	}
	head := fmt.Sprintf("%s (%d %s, default: %s", group.Provider, len(group.Logins), noun, def)
	if group.Default == LoginRotateSelection || group.Choice.Rotate {
		head += ", strategy: " + group.Strategy
	}
	if group.OnExhausted == LoginOnExhaustedRotate {
		head += ", on_exhausted: rotate"
	}
	return head + "):"
}

// Prints one provider's logins for icebath auth list
func (c *authCLI) printLoginGroup(provider string) {
	group := ListLogins(c.env, provider)
	if len(group.Logins) == 0 {
		return
	}
	fmt.Fprintln(c.out, authLoginHeader(group))
	for i, l := range group.Logins {
		fmt.Fprintln(c.out, authLoginRow(i+1, l))
	}
	fmt.Fprintln(c.out)
}

// Provider and one positional target of a login subcommand
func (c *authCLI) loginTarget() (string, string, error) {
	provider := c.normalizeProvider(authTextOr(c.ns["provider"]))
	if provider == "" {
		return "", "", authExit("Provider is required.")
	}
	return provider, strings.TrimSpace(authTextOr(c.ns["target"])), nil
}

// icebath auth use <provider> <login|@rotate>, the profile default login of the provider
func (c *authCLI) use() error {
	provider, target, err := c.loginTarget()
	if err != nil {
		return err
	}
	if target == "" {
		return authExit(fmt.Sprintf("Name a login or @rotate. Run `icebath auth list %s` to see them.", provider))
	}
	value, err := SetLoginDefault(c.env, provider, target)
	if err != nil {
		return authExit(err.Error())
	}
	if value == LoginRotateSelection {
		fmt.Fprintf(c.out, "%s now rotates through its logins marked rotate (strategy %s).\n", provider, GetPoolStrategy(c.env.loginCtx(), provider))
		return nil
	}
	row, _ := FindLogin(c.env, provider, value)
	fmt.Fprintf(c.out, "%s now uses the login %s by default. Sessions pinned to another login keep theirs.\n", provider, row.LoginName())
	return nil
}

// icebath auth rename <provider> <login> <new-name>
func (c *authCLI) rename() error {
	provider, target, err := c.loginTarget()
	if err != nil {
		return err
	}
	renamed, err := RenameLogin(c.env, provider, target, strings.TrimSpace(authTextOr(c.ns["name"])))
	if err != nil {
		return authExit(err.Error())
	}
	fmt.Fprintf(c.out, "Renamed %s login %s to %s.\n", provider, renamed.ID, renamed.Name)
	return nil
}

// icebath auth rotation <provider> [on|off|<strategy>] [logins...] [--on-exhausted fail|rotate]
func (c *authCLI) rotation() error {
	provider := c.normalizeProvider(authTextOr(c.ns["provider"]))
	if provider == "" {
		return authExit("Provider is required.")
	}
	mode := strings.ToLower(strings.TrimSpace(authTextOr(c.ns["mode"])))
	if onExhausted := strings.TrimSpace(authTextOr(c.ns["on_exhausted"])); onExhausted != "" {
		if err := SetLoginOnExhausted(c.env, provider, onExhausted); err != nil {
			return authExit(err.Error())
		}
		fmt.Fprintf(c.out, "logins.%s.on_exhausted: %s\n", provider, LoginOnExhausted(c.env.loginCtx(), provider))
	}
	targets := authStrings(c.ns["logins"])
	switch {
	case mode == "":
		group := ListLogins(c.env, provider)
		fmt.Fprintln(c.out, authLoginHeader(group))
		for _, l := range group.Logins {
			state := "off"
			if l.Rotate {
				state = "on"
			}
			fmt.Fprintf(c.out, "  %-20s rotate=%s\n", l.Name, state)
		}
		return nil
	case SupportedPoolStrategies[mode]:
		if err := SetLoginStrategy(c.env, provider, mode); err != nil {
			return authExit(err.Error())
		}
		fmt.Fprintf(c.out, "Set %s rotation strategy to %s.\n", provider, mode)
		return nil
	case mode == "on" || mode == "off":
	default:
		return authExit(fmt.Sprintf("Unknown rotation mode %q. Use on, off, fill_first, round_robin, least_used or random.", mode))
	}
	on := mode == "on"
	if len(targets) == 0 {
		if !on {
			if LoginDefault(c.env.loginCtx(), provider) != LoginRotateSelection {
				fmt.Fprintf(c.out, "%s is not rotating.\n", provider)
				return nil
			}
			id := currentLoginID(c.env, provider)
			if id == "" {
				return authExit(fmt.Sprintf("No %s login is ready to keep. Pick one with `icebath auth use %s <login>`.", provider, provider))
			}
			if _, err := SetLoginDefault(c.env, provider, id); err != nil {
				return authExit(err.Error())
			}
			row, _ := FindLogin(c.env, provider, id)
			fmt.Fprintf(c.out, "%s stopped rotating and uses %s. Logins keep their rotate marks for next time.\n", provider, row.LoginName())
			return nil
		}
		for _, l := range ListLogins(c.env, provider).Logins {
			targets = append(targets, l.ID)
		}
	}
	for _, target := range targets {
		updated, err := SetLoginRotation(c.env, provider, target, on)
		if err != nil {
			return authExit(err.Error())
		}
		fmt.Fprintf(c.out, "%s login %s: rotate=%s\n", provider, updated.Name, mode)
	}
	if on {
		if _, err := SetLoginDefault(c.env, provider, LoginRotateSelection); err != nil {
			return authExit(err.Error())
		}
		fmt.Fprintf(c.out, "%s now rotates through its rotate logins (strategy %s).\n", provider, GetPoolStrategy(c.env.loginCtx(), provider))
	}
	return nil
}

// Names a login an auth add just stored, applies --rotate and --default, and says how to use it
func (c *authCLI) nameAddedLogin(provider string, entry PooledCredential, previousActive string) error {
	named, err := NameNewLogin(c.env, provider, entry.ID, strings.TrimSpace(authTextOr(c.ns["name"])), nsFlag(c.ns, "rotate"), previousActive)
	if err != nil {
		return authExit(err.Error())
	}
	fmt.Fprintf(c.out, "Added the %s login %s.\n", provider, named.Name)
	if nsFlag(c.ns, "default") {
		if _, err := SetLoginDefault(c.env, provider, named.ID); err != nil {
			return authExit(err.Error())
		}
		fmt.Fprintf(c.out, "%s now uses %s by default.\n", provider, named.Name)
		return nil
	}
	if previousActive != "" && previousActive != named.ID {
		fmt.Fprintf(c.out, "The default login is unchanged. Use this one with `icebath auth use %s %s` or `/model <model> --login %s`.\n", provider, named.Name, named.Name)
	}
	return nil
}

// Logins the provider's pool holds and the one its requests use, read before a sign in so the login it stores can be told apart
func (c *authCLI) storedLogins(provider string) ([]PooledCredential, string) {
	if provider == "" {
		return nil, ""
	}
	return LoadPool(c.env, provider).Entries(), currentLoginID(c.env, provider)
}

// Login a sign in left in the provider's pool since before: the row that is new, else the row that holds a new token
func loginStoredSince(env PoolEnv, provider string, before []PooledCredential) (entry PooledCredential, isNew, found bool) {
	held := map[string]string{}
	for _, e := range before {
		held[e.ID] = e.AccessToken
	}
	var renewed *PooledCredential
	for _, e := range LoadPool(env, provider).Entries() {
		token, existed := held[e.ID]
		if !existed {
			return e, true, true
		}
		if token != e.AccessToken && renewed == nil {
			row := e
			renewed = &row
		}
	}
	if renewed != nil {
		return *renewed, false, true
	}
	return PooledCredential{}, false, false
}

// Flags of auth add that shape the stored login, as the user typed them
func (c *authCLI) namingFlags() []string {
	var asked []string
	if strings.TrimSpace(authTextOr(c.ns["name"])) != "" {
		asked = append(asked, "--name")
	}
	for _, flag := range []string{"rotate", "default"} {
		if nsFlag(c.ns, flag) {
			asked = append(asked, "--"+flag)
		}
	}
	if _, ok := authWantedPriority(c.ns); ok {
		asked = append(asked, "--priority")
	}
	return asked
}

// Applies --name, --rotate, --default and --priority to the login a plugin's own sign in stored and says what was added; says which flags did not apply when the plugin keeps its sign in outside the provider's logins
func (c *authCLI) adoptStoredLogin(provider string, before []PooledCredential, previousActive string) error {
	entry, isNew, found := loginStoredSince(c.env, provider, before)
	if !found {
		if asked := c.namingFlags(); len(asked) > 0 {
			fmt.Fprintf(c.out, "%s keeps this sign-in itself, so %s did not apply.\n", provider, strings.Join(asked, ", "))
		}
		return nil
	}
	if isNew {
		if err := c.nameAddedLogin(provider, entry, previousActive); err != nil {
			return err
		}
		return c.placeAddedLogin(provider, entry.ID)
	}
	name := entry.LoginName()
	if wanted := strings.TrimSpace(authTextOr(c.ns["name"])); wanted != "" && wanted != name {
		renamed, err := RenameLogin(c.env, provider, entry.ID, wanted)
		if err != nil {
			return authExit(err.Error())
		}
		name = renamed.Name
	}
	if nsFlag(c.ns, "rotate") {
		if _, err := SetLoginRotation(c.env, provider, entry.ID, true); err != nil {
			return authExit(err.Error())
		}
	}
	fmt.Fprintf(c.out, "Signed in again to the %s login %s.\n", provider, name)
	if nsFlag(c.ns, "default") {
		if _, err := SetLoginDefault(c.env, provider, entry.ID); err != nil {
			return authExit(err.Error())
		}
		fmt.Fprintf(c.out, "%s now uses %s by default.\n", provider, name)
	}
	return c.placeAddedLogin(provider, entry.ID)
}

// Refuses an invalid or taken --name before any sign in runs
func (c *authCLI) checkRequestedLoginName(provider string) error {
	name := strings.TrimSpace(authTextOr(c.ns["name"]))
	if name == "" {
		return nil
	}
	if err := checkNewLoginName(c.env, provider, name); err != nil {
		return authExit(err.Error())
	}
	return nil
}

// Login a plain logout removes, the pinned profile default else the one in use, rotating when the default is the rotation set
func logoutTargetLogin(env PoolEnv, provider string) (row PooledCredential, rotating, found bool) {
	pool := LoadPool(env, provider)
	if !pool.HasCredentials() {
		return PooledCredential{}, false, false
	}
	entries := ensureLoginNames(env, provider, pool.Entries())
	profile := profileLoginEnv(env)
	choice := ResolveLoginChoice(profile, provider, entries)
	if choice.Rotate {
		return PooledCredential{}, true, false
	}
	id := choice.LoginID
	if id == "" {
		id = currentLoginID(profile, provider)
	}
	for _, e := range entries {
		if e.ID == id {
			return e, false, true
		}
	}
	return PooledCredential{}, false, false
}

// Removes the login a plain logout targets and returns how many logins remain
func logoutLogin(env PoolEnv, provider string, row PooledCredential, out io.Writer) (int, error) {
	removal, err := RemoveLogin(env, provider, row.ID)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(out, "Removed the %s login %s.\n", provider, row.LoginName())
	for _, line := range removal.Cleaned {
		fmt.Fprintln(out, line)
	}
	remaining := LoadPool(env, provider).EntryCount()
	if remaining > 0 && removal.NewDefault != "" {
		if next, err := FindLogin(env, provider, removal.NewDefault); err == nil {
			fmt.Fprintf(out, "%s now uses the login %s by default.\n", provider, next.LoginName())
		}
	}
	return remaining, nil
}

// Suppresses every env source of a provider so a logout --all stays logged out when the variables remain set
func suppressProviderEnvSources(env PoolEnv, provider string) error {
	pconfig, ok := ProfileFor(provider)
	if !ok {
		return nil
	}
	vars := append([]string(nil), pconfig.KeyEnv...)
	if provider == "anthropic" {
		vars = []string{"ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}
	}
	for _, name := range envKeyVarCandidates(env, vars, nil) {
		if GetEnvPreferDotenv(env, name) == "" {
			continue
		}
		if err := env.Store.SuppressCredentialSource(provider, "env:"+name); err != nil {
			return err
		}
	}
	return nil
}
