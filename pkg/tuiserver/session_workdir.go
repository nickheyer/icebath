package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// A Windows drive path rewritten to its WSL mount on POSIX hosts, user syntax expanded
func normalizeCompletionPath(pathPart string) string {
	expanded := pathutil.ExpandUser(pathPart)
	if runtime.GOOS != "windows" {
		normalized := strings.ReplaceAll(expanded, "\\", "/")
		if len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/' && unicode.IsLetter(rune(normalized[0])) {
			return fmt.Sprintf("/mnt/%s/%s", strings.ToLower(normalized[:1]), normalized[3:])
		}
	}
	return expanded
}

// Working directory path completion starts from in the profile of ctx, a named profile landing in its own workspace or home
func completionCwd(ctx context.Context) (string, error) {
	profileHomePath := ""
	if h := home.Dir(ctx); h != "" && resolveLax(h) != resolveLax(launchHome()) {
		profileHomePath = h
	}
	namedSSH := profileHomePath != "" && cwdIsRemote(ctx, profileHomePath)
	raw := ""
	for _, candidate := range []func() string{
		func() string { return profileDefaultCwd(ctx, profileHomePath) },
		func() string {
			if namedSSH {
				return "~"
			}
			return ""
		},
		func() string { return launchConfiguredCwd(ctx) },
		func() string { return os.Getenv("TERMINAL_CWD") },
		func() string { return sandboxWorkspaceCwd(ctx, "") },
		func() string { cwd, _ := os.Getwd(); return cwd },
	} {
		if raw != "" {
			break
		}
		raw = candidate()
	}
	if namedSSH {
		if isRemoteCwdShape(raw) {
			return raw, nil
		}
		if declared := declaredRemoteProfileCwd(ctx, profileHomePath); declared != "" {
			return declared, nil
		}
		return "~", nil
	}
	if profileHomePath == "" && strings.HasPrefix(raw, "~") && cwdIsRemote(ctx, "") {
		return raw, nil
	}
	if resolved, err := filepath.Abs(pathutil.ExpandUser(raw)); err == nil && isDir(resolved) {
		return resolved, nil
	}
	if profileHomePath == "" && isRemoteCwdShape(raw) && cwdIsRemote(ctx, "") {
		return raw, nil
	}
	if isContainerPath(raw) && boundTerminalBackend(ctx, profileHomePath) != "local" {
		return raw, nil
	}
	cwd, err := os.Getwd()
	return cwd, err
}

// Effective terminal policy of a named profile, empty for the launch profile or an unreadable one
func profileTerminalPolicy(ctx context.Context, profileHome string) tools.TerminalPolicy {
	if profileHome == "" {
		return tools.TerminalPolicy{}
	}
	policy, err := tools.BuildProfileTerminalScope(ctx, profileHome, nil)
	if err != nil {
		var unavailable *tools.TerminalPolicyUnavailable
		if errors.As(err, &unavailable) {
			return tools.TerminalPolicy{}
		}
		panic(err)
	}
	return policy
}

// Lowercased backend of a terminal policy, local when unset
func policyBackend(policy tools.TerminalPolicy) string {
	if backend := strings.ToLower(strings.TrimSpace(policy["TERMINAL_ENV"])); backend != "" {
		return backend
	}
	return "local"
}

// Terminal backend of the profile a session is bound to, the launch backend for the launch profile
func boundTerminalBackend(ctx context.Context, profileHome string) string {
	if profileHome != "" {
		return policyBackend(profileTerminalPolicy(ctx, profileHome))
	}
	return effectiveTerminalBackend(ctx)
}

// Whether the bound profile working directory lives on another host over ssh
func cwdIsRemote(ctx context.Context, profileHome string) bool {
	return boundTerminalBackend(ctx, profileHome) == "ssh"
}

// Terminal cwd a named ssh profile declares, unchecked against this host, empty otherwise
func declaredRemoteProfileCwd(ctx context.Context, profileHome string) string {
	policy := profileTerminalPolicy(ctx, profileHome)
	raw := strings.TrimSpace(policy["TERMINAL_CWD"])
	if policyBackend(policy) == "ssh" && isRemoteCwdShape(raw) {
		return raw
	}
	return ""
}

// An ssh working directory the remote shell resolves, tilde relative or absolute
func isRemoteCwdShape(raw string) bool {
	return config.IsSSHRemoteTildeCwd("ssh", raw) || filepath.IsAbs(raw)
}

// An absolute POSIX path, the only shape a container working directory takes
func isContainerPath(raw string) bool {
	return strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//")
}

// Configured container backend cwd missing on the host, empty for local or ssh backends or a host dir
func sandboxWorkspaceCwd(ctx context.Context, profileHome string) string {
	var backend, raw string
	if profileHome != "" {
		policy := profileTerminalPolicy(ctx, profileHome)
		backend, raw = policyBackend(policy), strings.TrimSpace(policy["TERMINAL_CWD"])
	} else {
		backend = effectiveTerminalBackend(ctx)
		raw = strings.TrimSpace(os.Getenv("TERMINAL_CWD"))
		if raw == "" {
			raw = strings.TrimSpace(settings.WorkingFolder.Get(ctx))
		}
	}
	if backend == "local" || backend == "ssh" || !isContainerPath(raw) || isDir(raw) {
		return ""
	}
	return raw
}

// Configured workspace of a named profile, its remote ssh dir, a host dir, else a container dir
func profileWorkspaceCwd(ctx context.Context, profileHome string) string {
	if cwd := declaredRemoteProfileCwd(ctx, profileHome); cwd != "" {
		return cwd
	}
	if cwd := profileConfiguredCwd(ctx, profileHome); cwd != "" {
		return cwd
	}
	if profileHome != "" {
		return sandboxWorkspaceCwd(ctx, profileHome)
	}
	return ""
}

// Landing cwd of a profile session, its workspace else a named local profile home, never the launch cwd
func profileDefaultCwd(ctx context.Context, profileHome string) string {
	if workspace := profileWorkspaceCwd(ctx, profileHome); workspace != "" {
		return workspace
	}
	if profileHome != "" && boundTerminalBackend(ctx, profileHome) == "local" {
		return profileHome
	}
	return ""
}

// Active terminal backend name of the profile in ctx
func effectiveTerminalBackend(ctx context.Context) string {
	return strings.ToLower(settings.WhereCommandsRun.Get(ctx))
}
