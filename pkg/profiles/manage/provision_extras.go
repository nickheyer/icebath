package manage

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Auth modes an MCP server of a create may name
var mcpCreateAuthModes = map[string]bool{"none": true, "header": true, "oauth": true}

// Names of the directories holding a SKILL.md anywhere under a skills tree, in walk order
func installedSkillNames(skillsRoot string) []string {
	var names []string
	if !isDir(skillsRoot) {
		return names
	}
	_ = filepath.WalkDir(skillsRoot, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Name() == "SKILL.md" {
			names = append(names, filepath.Base(filepath.Dir(path)))
		}
		return nil
	})
	return names
}

// Disables every installed skill of a profile not named in keep and returns how many were newly disabled
func DisableUnselectedSkills(ctx context.Context, profileDir string, keep []string) (int, error) {
	keepSet := map[string]bool{}
	for _, name := range keep {
		if stripped := pystr.Strip(name); stripped != "" {
			keepSet[stripped] = true
		}
	}
	scoped, unlock := provider.LockConfigMutation(provider.ProfileConfigScope(ctx, profileDir, ""))
	defer unlock()
	installed := installedSkillNames(filepath.Join(profileDir, "skills"))
	cfg, err := loadScopedConfig(scoped)
	if err != nil {
		return 0, err
	}
	disabled := skills.GetDisabledSkills(cfg, "")
	newly := 0
	for _, name := range installed {
		if !keepSet[name] && !disabled[name] {
			disabled[name] = true
			newly++
		}
	}
	if newly == 0 {
		return 0, nil
	}
	if err := skills.SaveDisabledSkills(scoped, home.Dir(scoped), cfg, disabled, ""); err != nil {
		return 0, err
	}
	return newly, nil
}

// Config name, entry and bearer token of an MCP server a create asks for, the entry never holding the token
func normalizeMCPServerCreate(server MCPServerCreate) (string, map[string]any, *string, error) {
	refuse := func(format string, args ...any) (string, map[string]any, *string, error) {
		return "", nil, nil, &profiles.ValueError{Msg: fmt.Sprintf(format, args...)}
	}
	name := pystr.Strip(server.Name)
	if name == "" {
		return refuse("Server name is required")
	}
	url, command := pystr.Strip(server.URL), pystr.Strip(server.Command)
	auth := server.Auth
	if auth == "" {
		auth = "none"
	}
	auth = pystr.Lower(pystr.Strip(auth))
	if (url != "") == (command != "") {
		return refuse("Provide exactly one of URL (HTTP/SSE) or command (stdio)")
	}
	if !mcpCreateAuthModes[auth] {
		return refuse("Unsupported auth mode: %s", auth)
	}
	entry := map[string]any{}
	var order []string
	set := func(key string, value any) {
		entry[key] = value
		order = append(order, key)
	}
	if url != "" {
		if len(server.Args) > 0 {
			return refuse("Arguments are only supported for stdio MCP servers")
		}
		if len(server.Env) > 0 {
			return refuse("Environment variables are only supported for stdio MCP servers")
		}
		switch {
		case auth == "header":
			normalized := ""
			if server.BearerToken != nil && *server.BearerToken != "" {
				normalized = mcp.StripBearerPrefix(*server.BearerToken)
			}
			if normalized == "" || pystr.Lower(normalized) == "bearer" {
				return refuse("Bearer token is required")
			}
			set("headers", mcp.BearerAuthHeaders(name))
		case server.BearerToken != nil:
			return refuse("Bearer token requires header authentication")
		}
		set("url", url)
		if auth == "oauth" {
			set("auth", "oauth")
		}
	} else {
		if auth != "none" || server.BearerToken != nil {
			return refuse("HTTP authentication is not supported for stdio MCP servers")
		}
		set("command", command)
		if len(server.Args) > 0 {
			args := make([]any, len(server.Args))
			for i, arg := range server.Args {
				args[i] = arg
			}
			set("args", args)
		}
		if len(server.Env) > 0 {
			env := make(map[string]any, len(server.Env))
			for key, value := range server.Env {
				env[key] = value
			}
			set("env", env)
		}
	}
	config.SetMappingKeys(entry, order)
	if issues := mcp.ValidateServerEntry(name, entry); len(issues) > 0 {
		return refuse("Server '%s' rejected: %s", name, strings.Join(issues, "; "))
	}
	return name, entry, server.BearerToken, nil
}

// Writes the MCP servers of a create into a profile's config in one save, an invalid one skipped, and returns how many were written
func WriteProfileMCPServers(ctx context.Context, profileDir string, servers []MCPServerCreate) (int, error) {
	scoped, unlock := provider.LockConfigMutation(provider.ProfileConfigScope(ctx, profileDir, ""))
	defer unlock()
	cfg, err := loadScopedConfig(scoped)
	if err != nil {
		return 0, err
	}
	section, held := cfg["mcp_servers"]
	if !held {
		section = map[string]any{}
		cfg["mcp_servers"] = section
	}
	configured, isMapping := section.(map[string]any)
	if !isMapping {
		return 0, fmt.Errorf("config mcp_servers is %s where a mapping was expected", errchain.Kind(section))
	}
	written := 0
	for _, server := range servers {
		name, entry, bearerToken, err := normalizeMCPServerCreate(server)
		if err != nil {
			shown := pystr.Strip(server.Name)
			if shown == "" {
				shown = "<unnamed>"
			}
			pkgLog().Warn(fmt.Sprintf("Profile-create: skipping MCP server '%s': %s", shown, err))
			continue
		}
		if bearerToken != nil {
			headers, err := mcp.SaveBearerAuthToken(scoped, name, *bearerToken)
			if err != nil {
				return 0, err
			}
			entry["headers"] = headers
		}
		_, replaced := configured[name]
		configured[name] = entry
		if !replaced {
			config.AppendMappingKey(configured, name)
		}
		written++
	}
	switch {
	case written > 0:
		err = config.SaveConfig(scoped, home.Dir(scoped), cfg, config.SaveOptions{})
	case len(configured) == 0:
		delete(cfg, "mcp_servers")
		err = config.SaveConfig(scoped, home.Dir(scoped), cfg, config.SaveOptions{})
	}
	if err != nil {
		return 0, err
	}
	return written, nil
}
