package tools

import (
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"unicode/utf8"
)

var (
	// Variables of a client's shell that set how its sessions behave rather than what their commands run with
	clientBehaviour = []string{"ICEBATH_EPHEMERAL_SYSTEM_PROMPT", "ICEBATH_PREFILL_MESSAGES_FILE", "ICEBATH_INFERENCE_PROVIDER", "ICEBATH_ENABLE_PROJECT_PLUGINS"}
	// Behaviour variables that name something of the client's machine, kept only of a client on this one
	hostBehaviour = []string{"ICEBATH_PREFILL_MESSAGES_FILE", "ICEBATH_ENABLE_PROJECT_PLUGINS"}
	// Toolchain of a shell, by name and by prefix
	toolchainNames    = []string{"PATH", "VIRTUAL_ENV", "GOPATH", "GOROOT", "CARGO_HOME", "RUSTUP_HOME", "JAVA_HOME"}
	toolchainPrefixes = []string{"CONDA_", "NODE_", "NVM_", "PYENV_"}
	// Fragments of a variable name that mark it as a credential
	credentialFragments = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PASSWD", "AUTH", "DSN", "WEBHOOK", "CREDS", "BEARER", "APIKEY",
		"COOKIE"}
)

// Most variables and characters of a client's environment a session takes
const (
	clientMaxNames = 256
	clientMaxChars = 256 * 1024
)

var ErrClient = errors.New("The client's environment must be at most 256 variable names with string values, 256 KiB in all; it was not applied.")

// Rules a session keeps a client's variables by: every behaviour variable, those naming something of a machine only of a client on this
// one, and of a client on this machine its toolchain and the extra names given
func clientRules(local bool, extra []string) *Client {
	rules := &Client{Local: local, Behaviour: []string{}, Names: []string{}, Prefixes: []string{}, Secret: slices.Clone(credentialFragments)}
	for _, name := range clientBehaviour {
		if local || !slices.Contains(hostBehaviour, name) {
			rules.Behaviour = append(rules.Behaviour, name)
		}
	}
	if !local {
		return rules
	}
	for _, name := range append(slices.Clone(toolchainNames), extra...) {
		if name = strings.TrimSpace(name); name != "" && !slices.Contains(rules.Names, name) {
			rules.Names = append(rules.Names, name)
		}
	}
	slices.Sort(rules.Names)
	rules.Prefixes = slices.Clone(toolchainPrefixes)
	return rules
}

// Whether a variable may travel from a client's shell into commands: no credential by the fragments, nothing of icebath's own
func travels(name string, fragments []string) bool {
	upper := strings.ToUpper(name)
	for _, fragment := range fragments {
		if strings.Contains(upper, fragment) {
			return false
		}
	}
	return !strings.HasPrefix(name, "ICEBATH_")
}

func prefixed(name string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) })
}

// Whether the rules keep the variable
func (c *Client) keeps(name string) bool {
	if slices.Contains(c.Behaviour, name) {
		return true
	}
	return travels(name, c.Secret) && (slices.Contains(c.Names, name) || prefixed(name, c.Prefixes))
}

// Variables of this process's shell a session it starts keeps as its client's: the behaviour variables, the toolchain and the extra names
// the profile's client variables setting lists
func ClientVariables(extra []string) map[string]string {
	rules := clientRules(true, extra)
	out := map[string]string{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok && name != "" && rules.keeps(name) {
			if _, seen := out[name]; !seen {
				out[name] = value
			}
		}
	}
	return out
}

// Client a session freezes from the variables its first message came with, kept by the rules of a client on this machine or of a remote
// one, extra naming the profile's client variables; ErrClient for more than a session takes
func FreezeClient(variables map[string]string, local bool, extra []string) (*Client, error) {
	if len(variables) > clientMaxNames {
		return nil, ErrClient
	}
	total := 0
	for name, value := range variables {
		total += utf8.RuneCountInString(name) + utf8.RuneCountInString(value)
	}
	if total > clientMaxChars {
		return nil, ErrClient
	}
	client := clientRules(local, extra)
	client.Env = map[string]string{}
	for name, value := range variables {
		if client.keeps(name) {
			client.Env[name] = value
		}
	}
	return client, nil
}

// Value of a behaviour variable for the session: its client's when it has one that may set it, else this process's; false when neither
// sets it
func (c *Client) Lookup(name string) (string, bool) {
	if c == nil || slices.Contains(hostBehaviour, name) && !c.Local {
		return os.LookupEnv(name)
	}
	value, ok := c.Env[name]
	return value, ok
}

// Environment the session's commands start from: base without the behaviour variables the client sets, and, for a client on this machine,
// with its toolchain in place of base's own, PATH replaced rather than dropped
func (c *Client) Toolchain(base map[string]string) map[string]string {
	env := maps.Clone(base)
	if c == nil {
		return env
	}
	for _, name := range clientBehaviour {
		if c.Local || !slices.Contains(hostBehaviour, name) {
			delete(env, name)
		}
	}
	if !c.Local {
		return env
	}
	for name := range env {
		if name != "PATH" && (slices.Contains(c.Names, name) && travels(name, c.Secret) || prefixed(name, c.Prefixes)) {
			delete(env, name)
		}
	}
	for name, value := range c.Env {
		if !slices.Contains(clientBehaviour, name) && travels(name, credentialFragments) {
			env[name] = value
		}
	}
	return env
}
