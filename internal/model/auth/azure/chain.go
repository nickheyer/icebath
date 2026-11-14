package azure

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
)

type member struct {
	key      string
	selector string
	group    string
	optIn    bool
}

var chainMembers = []member{
	{key: "environment", selector: "environmentcredential", group: "prod"},
	{key: "workload_identity", selector: "workloadidentitycredential", group: "prod"},
	{key: "managed_identity", selector: "managedidentitycredential", group: "prod"},
	{key: "shared_token_cache", group: "dev"},
	{key: "visual_studio_code", selector: "visualstudiocodecredential", group: "dev"},
	{key: "cli", selector: "azureclicredential", group: "dev"},
	{key: "powershell", selector: "azurepowershellcredential", group: "dev"},
	{key: "developer_cli", selector: "azuredeveloperclicredential", group: "dev"},
	{key: "interactive_browser", selector: "interactivebrowsercredential", optIn: true},
	{key: "broker", group: "dev"},
}

func chainSelection(cfg Config) (map[string]bool, error) {
	include := map[string]bool{}
	for _, m := range chainMembers {
		include[m.key] = !m.optIn
	}
	switch selected := strings.ToLower(strings.TrimSpace(os.Getenv(selectionVariable))); selected {
	case "":
	case "dev", "prod":
		for _, m := range chainMembers {
			include[m.key] = m.group == selected
		}
	default:
		valid := []string{"dev", "prod"}
		match := ""
		for _, m := range chainMembers {
			if m.selector == "" {
				continue
			}
			valid = append(valid, m.selector)
			if m.selector == selected {
				match = m.key
			}
		}
		if match == "" {
			sort.Strings(valid)
			return nil, &AuthError{Credential: nameDefault, Message: "Invalid value for " + selectionVariable + ": " + selected + ". Valid values are: " + strings.Join(valid, ", ") + "."}
		}
		for _, m := range chainMembers {
			include[m.key] = m.key == match
		}
	}
	if !cfg.ExcludeInteractiveBrowser {
		include["interactive_browser"] = true
	}
	return include, nil
}

type environment struct{ inner source }

func (environment) name() string { return nameEnvironment }

func (s environment) token(ctx context.Context, scope string, inChain bool) (accessToken, error) {
	if s.inner == nil {
		return accessToken{}, unavailable(nameEnvironment, "EnvironmentCredential authentication unavailable. Environment variables are not fully configured.\nVisit https://aka.ms/azsdk/python/identity/environmentcredential/troubleshoot to troubleshoot this issue.")
	}
	return s.inner.token(ctx, scope, inChain)
}

func (c *client) environmentSource(authority string) (source, error) {
	present := func(names ...string) bool {
		for _, name := range names {
			if _, ok := os.LookupEnv(name); !ok {
				return false
			}
		}
		return true
	}
	env := os.Getenv
	var inner source
	var err error
	switch {
	case present(clientVariable, secretVariable, tenantVariable):
		inner, err = c.clientSecret(env(tenantVariable), env(clientVariable), env(secretVariable), authority)
	case present(clientVariable, certPathVariable, tenantVariable):
		inner, err = c.certificate(env(tenantVariable), env(clientVariable), env(certPathVariable), env(certPassVariable), env(certChainVariable) != "", authority)
	case present(clientVariable, usernameVariable, passwordVariable):
		inner, err = c.usernamePassword(env(tenantVariable), env(clientVariable), env(usernameVariable), env(passwordVariable), authority)
	}
	if err != nil {
		return nil, err
	}
	if inner != nil {
		slog.Info("Environment is configured for " + inner.name())
		return environment{inner: inner}, nil
	}
	var set []string
	for _, name := range []string{clientVariable, certPathVariable, tenantVariable, secretVariable, usernameVariable, passwordVariable} {
		if _, ok := os.LookupEnv(name); ok {
			set = append(set, name)
		}
	}
	if len(set) > 0 {
		slog.Info("Incomplete environment configuration for EnvironmentCredential. These variables are set: " + strings.Join(set, ", "))
	} else {
		slog.Info("No environment configuration found.")
	}
	return environment{}, nil
}

type chain struct {
	members []source

	mu     sync.Mutex
	winner source
}

func (s *chain) name() string { return nameDefault }

func (s *chain) token(ctx context.Context, scope string, _ bool) (accessToken, error) {
	s.mu.Lock()
	winner := s.winner
	s.mu.Unlock()
	if winner != nil {
		return winner.token(ctx, scope, false)
	}
	failed := &ChainError{}
	for _, m := range s.members {
		token, err := m.token(ctx, scope, true)
		if err == nil {
			slog.Info("DefaultAzureCredential acquired a token from " + m.name())
			s.mu.Lock()
			s.winner = m
			s.mu.Unlock()
			return token, nil
		}
		failed.Attempts = append(failed.Attempts, Attempt{Credential: m.name(), Message: err.Error()})
		var skip *UnavailableError
		if !errors.As(err, &skip) {
			failed.Err = err
			break
		}
		if ctx.Err() != nil {
			failed.Err = ctx.Err()
			break
		}
	}
	slog.Warn(failed.Error())
	return accessToken{}, failed
}

func (i *Identity) defaultChain(cfg Config) (source, error) {
	include, err := chainSelection(cfg)
	if err != nil {
		return nil, err
	}
	env := os.Getenv
	authority := authorityHost(env)
	s := &chain{}
	if include["environment"] {
		src, err := i.c.environmentSource(authority)
		if err != nil {
			return nil, err
		}
		s.members = append(s.members, src)
	}
	if include["workload_identity"] && env(authorityVariable) != "" && env(tenantVariable) != "" && env(tokenFileVariable) != "" {
		src, err := i.c.workloadIdentity(env(tenantVariable), env(clientVariable), env(tokenFileVariable), authority)
		if err != nil {
			return nil, err
		}
		s.members = append(s.members, src)
	}
	if include["managed_identity"] {
		src, err := i.c.managedIdentity(env(clientVariable), !include["workload_identity"])
		if err != nil {
			return nil, err
		}
		s.members = append(s.members, src)
	}
	if include["shared_token_cache"] {
		s.members = append(s.members, i.c.sharedTokenCache(env(usernameVariable), env(tenantVariable), authority))
	}
	if include["visual_studio_code"] {
		src, err := i.visualStudioCode(env(tenantVariable))
		if err != nil {
			return nil, err
		}
		s.members = append(s.members, src)
	}
	if include["cli"] {
		s.members = append(s.members, azureCLI{})
	}
	if include["powershell"] {
		s.members = append(s.members, powerShell{})
	}
	if include["developer_cli"] {
		s.members = append(s.members, developerCLI{})
	}
	if include["interactive_browser"] {
		s.members = append(s.members, i.c.interactiveBrowser(env(tenantVariable), authority, i.host))
	}
	if include["broker"] {
		s.members = append(s.members, i.brokerSource(env(tenantVariable), authority))
	}
	return s, nil
}
