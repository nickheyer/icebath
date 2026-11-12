package plugins

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const (
	hostExtractTimeout   = 60 * time.Second
	hostProfileCacheVers = 2
)

// Model provider profile whose overridden methods and callable fields run in the plugin host
type HostedProfile struct {
	Name      string
	Type      string
	Fields    map[string]any
	Calls     map[string]HostedProfileCall
	PluginDir string
	Module    string
	// Why hermes would import this profile in process, its create_client then serving a live host client
	InProcess string
}

// One overridden method or callable field of a hosted profile
type HostedProfileCall struct {
	Async bool
	Sig   []wireParam
	Field bool
}

func (p *HostedProfile) String() string {
	return fmt.Sprintf("<plugin-host profile %s>", config.PyRepr(p.Name))
}

// Runs one overridden method or callable field in the plugin host of the active profile
func (p *HostedProfile) Call(ctx context.Context, attr string, args []any, kwargs map[string]any) (any, error) {
	m := GetPluginManager(ctx)
	if m == nil {
		return nil, &PluginHostUnavailable{Reason: "plugins.isolation is no longer 'host'; restart Icebath"}
	}
	host, err := m.pluginHost()
	if err != nil {
		return nil, err
	}
	return host.ProfileCall(ctx, p.PluginDir, p.Module, p.Name, attr, args, kwargs)
}

var hostedProfilesMu sync.Mutex
var hostedProfiles = map[string][]*HostedProfile{}

// Hosted profile proxies of a model provider plugin plus the reason each profile that must load in process cannot be hosted
func LoadHostedProfiles(ctx context.Context, pluginDir, moduleName string) ([]*HostedProfile, map[string]string, error) {
	all, err := loadProviderProfiles(ctx, pluginDir, moduleName)
	if err != nil {
		return nil, nil, err
	}
	inProcess := map[string]string{}
	hosted := []*HostedProfile{}
	for _, profile := range all {
		if profile.InProcess != "" {
			inProcess[profile.Name] = profile.InProcess
			continue
		}
		hosted = append(hosted, profile)
	}
	hostedProfilesMu.Lock()
	hostedProfiles[pluginDir] = hosted
	hostedProfilesMu.Unlock()
	return hosted, inProcess, nil
}

// Every profile a model provider plugin registers in extraction order, in process ones carrying their reason
func loadProviderProfiles(ctx context.Context, pluginDir, moduleName string) ([]*HostedProfile, error) {
	payload, err := cachedProfileExtraction(ctx, pluginDir, moduleName)
	if err != nil {
		return nil, err
	}
	if e := payload["error"]; config.PyTruthy(e) {
		return nil, &PluginHostUnavailable{Reason: config.PyStr(e)}
	}
	return hostedProfilesFrom(pluginDir, moduleName, payload["profiles"])
}

// Profile proxies of extraction entries, the in process reason kept on each profile that has one
func hostedProfilesFrom(pluginDir, moduleName string, entries any) ([]*HostedProfile, error) {
	out := []*HostedProfile{}
	for _, raw := range listOrEmpty(entries) {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		profile, err := hostedProfileFrom(pluginDir, moduleName, entry)
		if err != nil {
			return nil, err
		}
		if reason := entry["in_process"]; config.PyTruthy(reason) {
			profile.InProcess = config.PyStr(reason)
		}
		out = append(out, profile)
	}
	return out, nil
}

func hostedProfileFrom(pluginDir, moduleName string, entry map[string]any) (*HostedProfile, error) {
	p := &HostedProfile{Name: config.PyStr(entry["name"]), Type: wireStr(entry["type"]), Fields: map[string]any{},
		Calls: map[string]HostedProfileCall{}, PluginDir: pluginDir, Module: moduleName}
	for key, value := range mapOrEmpty(entry["fields"]) {
		decoded, err := wireDecode(value, nil)
		if err != nil {
			return nil, err
		}
		p.Fields[key] = decoded
	}
	for attr, raw := range mapOrEmpty(entry["calls"]) {
		meta := mapOrEmpty(raw)
		p.Calls[attr] = HostedProfileCall{Async: config.PyTruthy(meta["async"]), Sig: signatureFrom(meta["sig"]), Field: config.PyTruthy(meta["field"])}
	}
	return p, nil
}

// Reports whether a profile value is a hosted proxy whose code runs in the plugin host
func IsHostedProfile(profile any) bool {
	_, ok := profile.(*HostedProfile)
	return ok
}

// Env var names of every hosted profile that reads ambient credentials
func hostedAmbientEnvNames() []string {
	hostedProfilesMu.Lock()
	defer hostedProfilesMu.Unlock()
	names := map[string]bool{}
	for _, profiles := range hostedProfiles {
		for _, p := range profiles {
			if !config.PyTruthy(p.Fields["ambient_credentials"]) {
				continue
			}
			for _, name := range argStrings(p.Fields["env_vars"]) {
				names[name] = true
			}
		}
	}
	return sortedKeys(names)
}

// Digest of the cache version, interpreter, running version and every plugin file size and mtime
func profileFingerprint(pluginDir, python string) string {
	digest := sha256.New()
	fmt.Fprintf(digest, "%d|%s", hostProfileCacheVers, python)
	digest.Write([]byte(RunningVersion))
	var files []string
	_ = filepath.WalkDir(pluginDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	for _, p := range files {
		rel, _ := filepath.Rel(pluginDir, p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		skip := false
		for _, part := range parts {
			if part == "__pycache__" || part == ".git" {
				skip = true
			}
		}
		info, err := os.Stat(p)
		if skip || err != nil || !info.Mode().IsRegular() {
			continue
		}
		fmt.Fprintf(digest, "%s|%d|%d", rel, info.Size(), info.ModTime().UnixNano())
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// Extraction payload cached under the home by the plugin fingerprint and module name
func cachedProfileExtraction(ctx context.Context, pluginDir, moduleName string) (map[string]any, error) {
	resolved, err := filepath.Abs(pluginDir)
	if err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = r
	}
	pathKey := sha256.Sum256([]byte(resolved))
	cache := filepath.Join(home.CacheDir(home.Dir(ctx)), "plugin_host", "model-providers",
		fmt.Sprintf("%s-%s.json", filepath.Base(pluginDir), hex.EncodeToString(pathKey[:])[:12]))
	python, err := hostInterpreter(ctx)
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("profile extraction failed: %v", err)}, nil
	}
	fingerprint := profileFingerprint(pluginDir, python)
	if raw, err := os.ReadFile(cache); err == nil {
		var cached map[string]any
		if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &cached) == nil &&
			cached["fingerprint"] == fingerprint && cached["module_name"] == moduleName {
			if payload, ok := cached["payload"].(map[string]any); ok {
				return reloadPayload(payload)
			}
		}
	}
	payload := extractProfiles(ctx, resolved, moduleName, python)
	if !config.PyTruthy(payload["error"]) {
		if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
			return nil, err
		}
		body, err := pyJSONDumps(map[string]any{"fingerprint": fingerprint, "module_name": moduleName, "payload": payload})
		if err != nil {
			return nil, err
		}
		tmp := fmt.Sprintf("%s.%d.tmp", strings.TrimSuffix(cache, ".json"), os.Getpid())
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, cache); err != nil {
			return nil, err
		}
	}
	return payload, nil
}

// Cached payload reparsed with the wire parser so integers and floats keep their Python types
func reloadPayload(payload map[string]any) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	parsed, err := pyJSONLoads(body)
	if err != nil {
		return nil, err
	}
	out, _ := parsed.(map[string]any)
	return out, nil
}

// Imports the plugin in a throwaway credential free child and reads its profiles
func extractProfiles(ctx context.Context, pluginDir, moduleName, python string) map[string]any {
	h, err := filepath.Abs(home.Dir(ctx))
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("profile extraction failed: %v", err)}
	}
	env := map[string]string{}
	for _, key := range []string{"PATH", "LANG", "LC_ALL", "TZ", "SYSTEMROOT"} {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	userHome, _ := os.UserHomeDir()
	pythonPath, err := hostPythonPath(ctx, "")
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("profile extraction failed: %v", err)}
	}
	env["HOME"], env["USERPROFILE"], env["ICEBATH_HOME"], env["PYTHONPATH"], env[HostProcessEnv] = userHome, userHome, h, pythonPath, "1"
	if networkDenied(ctx) {
		env[HostDenyNetworkEnv] = "1"
	}
	launcher, err := hostLauncher(h)
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("profile extraction failed: %v", err)}
	}
	argv, err := hostChildArgv(launcher, python, "--extract-profiles", pluginDir, moduleName)
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("profile extraction failed: %v", err)}
	}
	runCtx, cancel := context.WithTimeout(context.Background(), hostExtractTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = envList(env), h
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if runCtx.Err() != nil {
		return map[string]any{"error": fmt.Sprintf("profile extraction failed: Command %s timed out after %s seconds",
			config.PyRepr(anyList(argv)), pyFloatRepr(hostExtractTimeout.Seconds()))}
	}
	var execErr *exec.Error
	var pathErr *fs.PathError
	if errors.As(runErr, &execErr) || errors.As(runErr, &pathErr) {
		return map[string]any{"error": fmt.Sprintf("profile extraction failed: %v", runErr)}
	}
	parsed, err := pyJSONLoads(stdout.Bytes())
	if payload, ok := parsed.(map[string]any); err == nil && ok {
		return payload
	}
	lines := strings.Split(strings.TrimSpace(strings.ToValidUTF8(stderr.String(), "�")), "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return map[string]any{"error": fmt.Sprintf("profile extraction exited %d: %s", exitCode(cmd.ProcessState), strings.Join(lines, " | "))}
}
