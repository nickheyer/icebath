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

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/pluginsdk"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Argv prefix the plugin host process runs under from plugins.host.launcher, a sandbox runner
func HostLauncher(ctx context.Context, cfg map[string]any) ([]string, error) {
	if cfg == nil {
		cfg = loadConfigOrEmpty(home.Dir(ctx))
	}
	host := asMap(pluginsSection(cfg)["host"])
	if host == nil {
		return []string{}, nil
	}
	launcher := host["launcher"]
	if !config.PyTruthy(launcher) {
		return []string{}, nil
	}
	if text, ok := launcher.(string); ok {
		return pyshlex.SplitPOSIX(text, false)
	}
	out := []string{}
	for _, part := range toList(launcher) {
		out = append(out, config.PyStr(part))
	}
	return out, nil
}

// Manifest of a plugin directory that declares a guest runtime, nil for any other directory
func guestManifestOfDir(dir string) *Manifest {
	file, err := nativeManifestFile(dir)
	if err != nil || file == "" {
		return nil
	}
	mf := ParseManifestFile(file, dir, "user", "")
	if mf == nil || manifestRuntime(mf) == "" {
		return nil
	}
	return mf
}

// Host running the code of a plugin directory, its own guest when it declares a runtime, else the profile host
func (m *Manager) hostForDir(dir string) (*PluginHost, error) {
	if mf := guestManifestOfDir(dir); mf != nil {
		return m.guestHost(mf)
	}
	return m.pluginHost()
}

// Exports hostForDir for the dashboard plugin api routes
func (m *Manager) HostForPluginDir(dir string) (*PluginHost, error) { return m.hostForDir(dir) }

// Digest of the cache version, this executable, the product version and every plugin file size and mtime
func profileFingerprint(pluginDir string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	fmt.Fprintf(digest, "%d|%s", hostProfileCacheVers, exe)
	digest.Write([]byte(pm.DunderVersion()))
	var files []string
	err = filepath.WalkDir(pluginDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(pluginDir, path)
		if err != nil {
			return err
		}
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			if part == "__pycache__" || part == ".git" {
				return nil
			}
		}
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(pluginDir, path)
		fmt.Fprintf(digest, "%s|%d|%d", rel, info.Size(), info.ModTime().UnixNano())
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// Extraction payload cached under the home by the plugin fingerprint and module name
func cachedProfileExtraction(ctx context.Context, pluginDir, moduleName string) (map[string]any, error) {
	resolved, err := pypath.Resolve(pluginDir)
	if err != nil {
		return nil, err
	}
	pathKey := sha256.Sum256([]byte(resolved))
	cache := filepath.Join(home.CacheDir(home.Dir(ctx)), "plugin_host", "model-providers",
		filepath.Base(pluginDir)+"-"+hex.EncodeToString(pathKey[:])[:12]+".json")
	fingerprint, err := profileFingerprint(pluginDir)
	if err != nil {
		return nil, err
	}
	if raw, err := os.ReadFile(cache); err == nil {
		var cached map[string]any
		if json.Unmarshal(stripBOM(raw), &cached) == nil && cached["fingerprint"] == fingerprint && cached["module_name"] == moduleName {
			if payload, ok := cached["payload"].(map[string]any); ok {
				return payload, nil
			}
		}
	}
	payload := extractProfiles(ctx, pluginDir, resolved, moduleName)
	if !config.PyTruthy(payload["error"]) {
		if err := os.MkdirAll(filepath.Dir(cache), 0o777); err != nil {
			return nil, err
		}
		doc, err := json.Marshal(map[string]any{"fingerprint": fingerprint, "module_name": moduleName, "payload": payload})
		if err != nil {
			return nil, err
		}
		tmp := strings.TrimSuffix(cache, ".json") + fmt.Sprintf(".%d.tmp", os.Getpid())
		if err := os.WriteFile(tmp, doc, 0o666); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, cache); err != nil {
			return nil, err
		}
	}
	return payload, nil
}

// Profiles a plugin registers, read from a throwaway credential free run of its guest
func extractProfiles(ctx context.Context, pluginDir, resolvedDir, moduleName string) map[string]any {
	mf := guestManifestOfDir(pluginDir)
	if mf == nil {
		return map[string]any{"error": errPythonSource().Error()}
	}
	spec, err := guestSpecOf(mf, home.Dir(ctx))
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("profile extraction failed: %v", err)}
	}
	var out, tail []byte
	code := 0
	if spec.runtime == RuntimeWasm {
		out, err = wasmGuestProfiles(ctx, spec)
	} else {
		out, tail, code, err = programGuestProfiles(ctx, spec, resolvedDir, moduleName)
	}
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("profile extraction failed: %v", err)}
	}
	var payload map[string]any
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil || payload == nil {
		lines := strings.Split(strings.TrimSpace(strings.ToValidUTF8(string(tail), "�")), "\n")
		if len(lines) > 3 {
			lines = lines[len(lines)-3:]
		}
		return map[string]any{"error": fmt.Sprintf("profile extraction exited %d: %s", code, strings.Join(lines, " | "))}
	}
	return payload
}

// Output, error tail and exit status of the entry program run with the extract profiles flag
func programGuestProfiles(ctx context.Context, spec *guestSpec, resolvedDir, moduleName string) ([]byte, []byte, int, error) {
	program, err := guestProgramPath(spec)
	if err != nil {
		return nil, nil, 0, err
	}
	launcher, err := HostLauncher(ctx, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	profileHome, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return nil, nil, 0, err
	}
	userHome, err := pypath.PathHome()
	if err != nil {
		return nil, nil, 0, err
	}
	env := map[string]string{}
	for _, key := range []string{"PATH", "LANG", "LC_ALL", "TZ", "SYSTEMROOT"} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	env["HOME"], env["USERPROFILE"], env["ICEBATH_HOME"], env[HostProcessEnv] = userHome, userHome, profileHome, "1"
	env[GuestPluginNameEnv], env[GuestPluginDirEnv] = spec.name, spec.dir
	argv := append(append(append([]string{}, launcher...), program), spec.entry[1:]...)
	argv = append(argv, pluginsdk.ExtractProfilesFlag, resolvedDir, moduleName)
	runCtx, cancel := context.WithTimeout(ctx, hostExtractTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = envList(env), profileHome
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if runCtx.Err() != nil {
		return nil, nil, 0, fmt.Errorf("Command '%s' timed out after %v seconds", strings.Join(argv, " "), hostExtractTimeout.Seconds())
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return nil, nil, 0, err
	}
	return stdout.Bytes(), stderr.Bytes(), exitCode(cmd.ProcessState), nil
}
