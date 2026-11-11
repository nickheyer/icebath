package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent config keys with the env variables config.yaml exports them to, config winning over .env
var agentEnvBridge = [][2]string{
	{"gateway_timeout", "ICEBATH_AGENT_TIMEOUT"},
	{"gateway_turn_lease_timeout", "ICEBATH_TURN_LEASE_TIMEOUT"},
	{"gateway_timeout_warning", "ICEBATH_AGENT_TIMEOUT_WARNING"},
	{"gateway_notify_interval", "ICEBATH_AGENT_NOTIFY_INTERVAL"},
	{"session_stall_timeout", "ICEBATH_SESSION_STALL_TIMEOUT"},
	{"restart_drain_timeout", "ICEBATH_RESTART_DRAIN_TIMEOUT"},
	{"cron_drain_timeout", "ICEBATH_CRON_DRAIN_TIMEOUT"},
	{"gateway_auto_continue_freshness", "ICEBATH_AUTO_CONTINUE_FRESHNESS"},
	{"gateway_startup_restore_drain_timeout", "ICEBATH_STARTUP_RESTORE_DRAIN_TIMEOUT"},
	{"gateway_startup_warmup_timeout", "ICEBATH_STARTUP_WARMUP_TIMEOUT"},
}

// Display config keys with the env variables the busy input handling reads
var displayEnvBridge = [][2]string{
	{"busy_input_mode", "ICEBATH_GATEWAY_BUSY_INPUT_MODE"},
	{"busy_text_mode", "ICEBATH_GATEWAY_BUSY_TEXT_MODE"},
	{"busy_ack_enabled", "ICEBATH_GATEWAY_BUSY_ACK_ENABLED"},
}

// Nested terminal config keys with the TERMINAL env variables the gateway bridge exports them to
var gatewayTerminalEnvMap = [][2]string{
	{"backend", "TERMINAL_ENV"},
	{"degraded_mode", "TERMINAL_DEGRADED_MODE"},
	{"cwd", "TERMINAL_CWD"},
	{"timeout", "TERMINAL_TIMEOUT"},
	{"home_mode", "TERMINAL_HOME_MODE"},
	{"lifetime_seconds", "TERMINAL_LIFETIME_SECONDS"},
	{"docker_image", "TERMINAL_DOCKER_IMAGE"},
	{"docker_forward_env", "TERMINAL_DOCKER_FORWARD_ENV"},
	{"modal_image", "TERMINAL_MODAL_IMAGE"},
	{"daytona_image", "TERMINAL_DAYTONA_IMAGE"},
	{"vercel_runtime", "TERMINAL_VERCEL_RUNTIME"},
	{"vercel_image", "TERMINAL_VERCEL_IMAGE"},
	{"ssh_host", "TERMINAL_SSH_HOST"},
	{"ssh_user", "TERMINAL_SSH_USER"},
	{"ssh_port", "TERMINAL_SSH_PORT"},
	{"ssh_key", "TERMINAL_SSH_KEY"},
	{"container_cpu", "TERMINAL_CONTAINER_CPU"},
	{"container_memory", "TERMINAL_CONTAINER_MEMORY"},
	{"container_disk", "TERMINAL_CONTAINER_DISK"},
	{"container_persistent", "TERMINAL_CONTAINER_PERSISTENT"},
	{"docker_volumes", "TERMINAL_DOCKER_VOLUMES"},
	{"docker_env", "TERMINAL_DOCKER_ENV"},
	{"docker_extra_args", "TERMINAL_DOCKER_EXTRA_ARGS"},
	{"docker_shm_size", "TERMINAL_DOCKER_SHM_SIZE"},
	{"docker_mount_cwd_to_workspace", "TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE"},
	{"docker_network", "TERMINAL_DOCKER_NETWORK"},
	{"docker_run_as_host_user", "TERMINAL_DOCKER_RUN_AS_HOST_USER"},
	{"docker_snap_compat", "TERMINAL_DOCKER_SNAP_COMPAT"},
	{"docker_persist_across_processes", "TERMINAL_DOCKER_PERSIST_ACROSS_PROCESSES"},
	{"docker_shared_container_key", "TERMINAL_DOCKER_SHARED_CONTAINER_KEY"},
	{"docker_orphan_reaper", "TERMINAL_DOCKER_ORPHAN_REAPER"},
	{"sandbox_dir", "TERMINAL_SANDBOX_DIR"},
	{"persistent_shell", "TERMINAL_PERSISTENT_SHELL"},
}

// Sets one process env variable, failing with the ValueError Python raises for a name it cannot hold
func bridgeSetenv(name, value string) error {
	if strings.ContainsRune(name, 0) || strings.ContainsRune(value, 0) {
		return &gateway.PyError{Kind: "ValueError", Msg: "embedded null byte"}
	}
	if name == "" || strings.Contains(name, "=") {
		return &gateway.PyError{Kind: "ValueError", Msg: "illegal environment variable name"}
	}
	return os.Setenv(name, value)
}

// Bridges nested terminal config to the TERMINAL env variables, config.yaml overriding .env for these keys
func bridgeTerminalConfigToEnv(terminalCfg map[string]any) error {
	backend := ""
	if configured := terminalCfg["backend"]; pyrepr.Truthy(configured) {
		backend = pyrepr.Str(configured)
	} else {
		backend = os.Getenv("TERMINAL_ENV")
	}
	backend = pystr.Lower(pystr.Strip(backend))
	for _, pair := range gatewayTerminalEnvMap {
		value, present := terminalCfg[pair[0]]
		if !present {
			continue
		}
		if pair[0] == "cwd" {
			switch pyrepr.Str(value) {
			case ".", "auto", "cwd":
				continue
			}
			if text, isStr := value.(string); isStr && !config.IsSSHRemoteTildeCwd(backend, pystr.Strip(text)) {
				value = pypath.Expanduser(text)
			}
		}
		if err := bridgeSetenv(pair[1], config.TerminalEnvValueText(value)); err != nil {
			return err
		}
	}
	return nil
}

// Bridges auxiliary model and endpoint overrides of vision, approval and every plugin task to the env
func bridgeAuxiliaryConfigToEnv(ctx context.Context, auxiliaryCfg map[string]any) error {
	keys := []string{"vision", "approval"}
	func() {
		defer func() { _ = recover() }()
		for _, entry := range plugins.GetPluginAuxiliaryTasks(ctx) {
			if key := entry["key"].(string); !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
	}()
	for _, taskKey := range keys {
		var taskAny any = map[string]any{}
		if configured, present := auxiliaryCfg[taskKey]; present {
			taskAny = configured
		}
		taskCfg, isDict := taskAny.(map[string]any)
		if !isDict {
			continue
		}
		field := func(name string) string {
			value, present := taskCfg[name]
			if !present {
				return ""
			}
			return pystr.Strip(pyrepr.Str(value))
		}
		upper := pystr.Upper(taskKey)
		if prov := field("provider"); prov != "" && prov != "auto" {
			if err := bridgeSetenv("AUXILIARY_"+upper+"_PROVIDER", prov); err != nil {
				return err
			}
		}
		for _, pair := range [][2]string{{"model", "MODEL"}, {"base_url", "BASE_URL"}, {"api_key", "API_KEY"}} {
			if value := field(pair[0]); value != "" {
				if err := bridgeSetenv("AUXILIARY_"+upper+"_"+pair[1], value); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Section of a config for the bridge, an empty mapping when the key is absent
func bridgeSection(cfg map[string]any, key string) any {
	if section, present := cfg[key]; present {
		return section
	}
	return map[string]any{}
}

// Exports config.yaml settings to the env variables their consumers read through the process environment
func bridgeConfigToEnv(ctx context.Context, cfg map[string]any) error {
	for _, key := range config.MappingKeys(cfg) {
		switch cfg[key].(type) {
		case string, bool, int, int64, float64:
			if _, set := os.LookupEnv(key); !set {
				if err := bridgeSetenv(key, pyrepr.Str(cfg[key])); err != nil {
					return err
				}
			}
		}
	}
	if terminalCfg, isDict := bridgeSection(cfg, "terminal").(map[string]any); isDict && len(terminalCfg) > 0 {
		if err := bridgeTerminalConfigToEnv(terminalCfg); err != nil {
			return err
		}
	}
	if auxiliaryCfg, isDict := bridgeSection(cfg, "auxiliary").(map[string]any); isDict && len(auxiliaryCfg) > 0 {
		if err := bridgeAuxiliaryConfigToEnv(ctx, auxiliaryCfg); err != nil {
			return err
		}
	}
	agentCfg := bridgeSection(cfg, "agent")
	gateway.BridgeMaxTurnsToEnv(agentCfg)
	gateway.BridgeSectionToEnv(agentCfg, agentEnvBridge)
	gateway.BridgeSectionToEnv(bridgeSection(cfg, "sessions"), gateway.SessionsEnvBridge())
	displayCfg := bridgeSection(cfg, "display")
	gateway.BridgeSectionToEnv(displayCfg, displayEnvBridge)
	if display, isDict := displayCfg.(map[string]any); isDict {
		if ack, present := display["busy_steer_ack_enabled"]; present {
			if _, set := os.LookupEnv("ICEBATH_GATEWAY_BUSY_STEER_ACK_ENABLED"); !set {
				if err := bridgeSetenv("ICEBATH_GATEWAY_BUSY_STEER_ACK_ENABLED", pyrepr.Str(ack)); err != nil {
					return err
				}
			}
		}
	}
	if tz, isStr := cfg["timezone"].(string); isStr && tz != "" {
		if err := bridgeSetenv("ICEBATH_TIMEZONE", pystr.Strip(tz)); err != nil {
			return err
		}
	}
	if security, isDict := bridgeSection(cfg, "security").(map[string]any); isDict && security["redact_secrets"] != nil {
		if err := bridgeSetenv("ICEBATH_REDACT_SECRETS", pystr.Lower(pyrepr.Str(security["redact_secrets"]))); err != nil {
			return err
		}
	}
	if gatewayCfg, isDict := bridgeSection(cfg, "gateway").(map[string]any); isDict {
		gateway.ApplyMediaPolicyEnv(ctx, cfg)
		if trust := gatewayCfg["trust_recent_files_seconds"]; trust != nil {
			if err := bridgeSetenv("ICEBATH_MEDIA_TRUST_RECENT_SECONDS", pyrepr.Str(trust)); err != nil {
				return err
			}
		}
		if timeout, present := gatewayCfg["platform_connect_timeout"]; present && pystr.Strip(os.Getenv("ICEBATH_GATEWAY_PLATFORM_CONNECT_TIMEOUT")) == "" {
			if err := bridgeSetenv("ICEBATH_GATEWAY_PLATFORM_CONNECT_TIMEOUT", pyrepr.Str(timeout)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Guards the one time import of the runner module, latched only once it ran whole
var runModuleImport struct {
	sync.Mutex
	done bool
}

// Runs once what importing the runner module does, the gateway marker, dotenv, config env bridge and startup warnings
func ImportRunModule(ctx context.Context) error {
	runModuleImport.Lock()
	defer runModuleImport.Unlock()
	if runModuleImport.done {
		return nil
	}
	os.Setenv("_ICEBATH_GATEWAY", "1")
	processHome := home.Process()
	if _, err := provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{Home: processHome, ProjectEnv: filepath.Join(pm.RepoRoot(), ".env")}); err != nil {
		return err
	}
	agentDefaults, _ := config.Defaults()["agent"].(map[string]any)
	os.Setenv("ICEBATH_TURN_LEASE_TIMEOUT", pyrepr.Str(agentDefaults["gateway_turn_lease_timeout"]))
	configPath := filepath.Join(processHome, "config.yaml")
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil {
		return err
	}
	cfg := map[string]any{}
	if exists {
		bridgeErr := func() error {
			loaded, err := gateway.LoadBridgeConfig(configPath)
			if err != nil {
				return err
			}
			cfg = loaded
			return bridgeConfigToEnv(ctx, cfg)
		}()
		if bridgeErr != nil {
			fmt.Fprintf(os.Stderr, "  Warning: config.yaml → env bridge failed: %s: %s\n", pyrepr.ExceptionName(bridgeErr), pyrepr.ExceptionMessage(bridgeErr))
			fmt.Fprintln(os.Stderr, "  Gateway will fall back to .env values, which may not match "+
				"your current config.yaml. Run `icebath doctor` to investigate.")
		}
	}
	warnOnFailure := func(label string, step func()) {
		defer func() {
			if failure := recover(); failure != nil {
				fmt.Fprintf(os.Stderr, "  Warning: %s failed: %v\n", label, failure)
			}
		}()
		step()
	}
	warnOnFailure("IPv4 preference application", func() {
		if network, isDict := bridgeSection(cfg, "network").(map[string]any); isDict && pyrepr.Truthy(network["force_ipv4"]) {
			home.ApplyIPv4Preference(true)
		}
	})
	warnOnFailure("config validation", func() {
		config.PrintConfigWarnings(ctx, home.Dir(ctx), nil, tools.RemovedBackendNote)
	})
	warnOnFailure("deprecation check", func() { config.WarnDeprecatedCwdEnvVars(os.Stderr, home.Dir(ctx)) })
	os.Setenv("ICEBATH_QUIET", "1")
	runModuleImport.done = true
	return nil
}
