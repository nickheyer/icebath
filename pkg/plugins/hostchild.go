package plugins

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

//go:embed all:hostchild
var hostChildFS embed.FS

const (
	HostProcessEnv        = "ICEBATH_PLUGIN_HOST_PROCESS"
	HostLogLevelEnv       = "ICEBATH_PLUGIN_HOST_LOG_LEVEL"
	HostDenyNetworkEnv    = "ICEBATH_PLUGIN_HOST_DENY_NETWORK"
	HostTerminalEnv       = "ICEBATH_PLUGIN_HOST_TERMINAL"
	PluginHostEntry       = "plugin-host"
	hostIsolationResource = "icebath_cli/plugin_isolation.py"
)

// Context methods the host refuses with the reason the live object cannot cross
var HostUnsupportedCtxMethods = map[string]string{
	"register_login_backend":           "browser-vault fills hold a server-local context manager",
	"register_browser_page_backend":    "browser-vault fill scripts carry credentials that stay in the Icebath process",
	"register_platform":                "platform adapters run on the gateway's event loop with native SDK clients",
	"register_platform_handler":        "the handler factory receives the platform's native SDK client",
	"register_telegram_handler":        "the handler factory receives the native Telegram Application",
	"register_slack_action_handler":    "the callback receives native slack_bolt ack/body objects",
	"register_dashboard_auth_provider": "dashboard auth is owned by the launch process, not a profile",
	"register_approval_transport":      "approval transports hold the live approval request/response channel",
	"post_message":                     "the returned handle is bound to the gateway's event loop and live adapter",
	"apost_message":                    "the returned handle is bound to the gateway's event loop and live adapter",
}

// Context methods the host skips with a warning
var HostSkippedCtxMethods = map[string]string{
	"register_cli_command": "`icebath <command>` subcommands are wired into the local CLI parser",
}

// Context facades whose methods run on the parent side object
var HostRemoteFacades = map[string]bool{
	"state": true, "llm": true, "platform_actions": true, "subagent_lifecycle": true, "delegation_backends": true,
}

// Registrars whose argument may be a plugin object, mapped to the provider base it stands in for
var HostObjectBases = map[string]string{
	"register_image_gen_provider":            "agent.image_gen_provider:ImageGenProvider",
	"register_video_gen_provider":            "agent.video_gen_provider:VideoGenProvider",
	"register_web_search_provider":           "agent.web_search_provider:WebSearchProvider",
	"register_browser_provider":              "agent.browser_provider:BrowserProvider",
	"register_terminal_environment_provider": "agent.terminal_env_provider:TerminalEnvironmentProvider",
	"register_secret_source":                 "agent.secret_sources.base:SecretSource",
	"register_tts_provider":                  "agent.tts_provider:TTSProvider",
	"register_transcription_provider":        "agent.transcription_provider:TranscriptionProvider",
	"register_delegation_provider":           "agent.delegation_provider:DelegationProvider",
	"register_context_engine":                "agent.context_engine:ContextEngine",
	"register_context_reference":             "agent.context_references:ContextReferenceProvider",
	"register_memory_provider":               "agent.memory_provider:MemoryProvider",
}

// Unsupported registrars the parent serves for an in process plugin, mapped to the base their object subclasses
var HostInProcessObjectBases = map[string]string{
	"register_login_backend":           "agent.vault_backends.base:LoginBackend",
	"register_browser_page_backend":    "agent.browser_page_backend:BrowserPageBackend",
	"register_dashboard_auth_provider": "hermes_cli.dashboard_auth:DashboardAuthProvider",
}

// Provider bases a category loader hands back as hosted objects though no ctx registration names them
var HostResultObjectBases = []string{"tools.computer_use.backend:ComputerUseProvider", "tools.computer_use.backend:ComputerUseBackend"}

// Hooks whose payload reaches hosted callbacks only as opaque values
var HostDegradedHooks = map[string]string{
	"pre_gateway_dispatch": "receives the live gateway runner and session store",
}

// Python module carrying the parent tables so the child refuses exactly what the parent refuses
func hostIsolationSource() string {
	var b strings.Builder
	dict := func(name string, table map[string]string) {
		fmt.Fprintf(&b, "%s: dict[str, str] = {\n", name)
		for _, k := range sortedKeys(table) {
			fmt.Fprintf(&b, "    %s: %s,\n", config.PyRepr(k), config.PyRepr(table[k]))
		}
		b.WriteString("}\n\n")
	}
	dict("HOST_UNSUPPORTED_CTX_METHODS", HostUnsupportedCtxMethods)
	dict("HOST_SKIPPED_CTX_METHODS", HostSkippedCtxMethods)
	facades := sortedKeys(HostRemoteFacades)
	quoted := make([]string, len(facades))
	for i, f := range facades {
		quoted[i] = config.PyRepr(f)
	}
	fmt.Fprintf(&b, "HOST_REMOTE_FACADES: frozenset[str] = frozenset({%s})\n\n", strings.Join(quoted, ", "))
	dict("HOST_OBJECT_BASES", HostObjectBases)
	dict("HOST_IN_PROCESS_OBJECT_BASES", HostInProcessObjectBases)
	results := make([]string, len(HostResultObjectBases))
	for i, ref := range HostResultObjectBases {
		results[i] = config.PyRepr(ref) + ","
	}
	fmt.Fprintf(&b, "HOST_RESULT_OBJECT_BASES: tuple[str, ...] = (%s)\n\n", strings.Join(results, " "))
	dict("HOST_DEGRADED_HOOKS", HostDegradedHooks)
	fmt.Fprintf(&b, "HOST_PROCESS_ENV = %s\n", config.PyRepr(HostProcessEnv))
	return b.String()
}

// Every child resource keyed by its path under the resource root
func hostChildFiles() (map[string][]byte, error) {
	files := map[string][]byte{hostIsolationResource: []byte(hostIsolationSource())}
	err := fs.WalkDir(hostChildFS, "hostchild", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := hostChildFS.ReadFile(p)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(p, "hostchild/")] = body
		return nil
	})
	return files, err
}

var hostChildMu sync.Mutex

// Writes the child resources once under a directory named by their digest and returns that directory
func materializeHostChild() (string, error) {
	files, err := hostChildFiles()
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	sum := sha256.New()
	for _, name := range names {
		fmt.Fprintf(sum, "%s\x00%d\x00", name, len(files[name]))
		sum.Write(files[name])
	}
	digest := hex.EncodeToString(sum.Sum(nil))[:16]
	dir := filepath.Join(home.CacheDir(home.Root()), "plugin-host", RunningVersion+"-"+digest)
	marker := filepath.Join(dir, ".complete")
	hostChildMu.Lock()
	defer hostChildMu.Unlock()
	if isFile(marker) {
		return dir, nil
	}
	tmp := fmt.Sprintf("%s.%d.tmp", dir, os.Getpid())
	_ = os.RemoveAll(tmp)
	for _, name := range names {
		target := filepath.Join(tmp, filepath.FromSlash(path.Clean(name)))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(target, files[name], 0o644); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, ".complete"), []byte(digest), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		if isFile(marker) {
			_ = os.RemoveAll(tmp)
			return dir, nil
		}
		_ = os.RemoveAll(dir)
		if err := os.Rename(tmp, dir); err != nil {
			_ = os.RemoveAll(tmp)
			return "", err
		}
	}
	return dir, nil
}

// Interpreter that runs the host child, swapped by tests for the system python3
var hostInterpreter = pluginInterpreter

// SwapHostInterpreter points host children at another interpreter resolver and returns the restore
func SwapHostInterpreter(fn func(context.Context) (string, error)) func() {
	previous := hostInterpreter
	hostInterpreter = fn
	return func() { hostInterpreter = previous }
}

// PYTHONPATH of a host child, the resource root and the plugin SDK ahead of the inherited path
func hostPythonPath(ctx context.Context, inherited string) (string, error) {
	root, err := materializeHostChild()
	if err != nil {
		return "", err
	}
	sdk, err := pluginSDKPythonPath(ctx)
	if err != nil {
		return "", err
	}
	parts := []string{}
	for _, p := range []string{root, sdk, inherited} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, string(os.PathListSeparator)), nil
}

// The plugins.host.launcher argv prefix of a home, a string split with shell rules
func hostLauncher(h string) ([]string, error) {
	hostCfg := asMap(pluginsSection(loadConfigOrEmpty(h))["host"])
	launcher := hostCfg["launcher"]
	if !config.PyTruthy(launcher) {
		return nil, nil
	}
	if s, ok := launcher.(string); ok {
		return tools.ShlexSplitCommand(s)
	}
	out := []string{}
	if items, ok := launcher.([]any); ok {
		for _, part := range items {
			out = append(out, config.PyStr(part))
		}
		return out, nil
	}
	return nil, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("'%s' object is not iterable", pyTypeName(launcher))}
}

var hostEntryDispatched atomic.Bool

// Records that this binary dispatches the plugin host entry, so a parent may re-execute it as the child
func MarkPluginHostEntry() { hostEntryDispatched.Store(true) }

// Argv that starts this binary as the plugin host child for the interpreter under the configured launcher
func hostChildArgv(launcher []string, python string, extra ...string) ([]string, error) {
	if !hostEntryDispatched.Load() {
		return nil, errors.New("the plugin host would re-execute a binary that never called pluginhost.DispatchPluginHostEntry, call it first in main or TestMain")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	argv := append(append([]string{}, launcher...), exe, PluginHostEntry, "--python", python)
	return append(argv, extra...), nil
}
