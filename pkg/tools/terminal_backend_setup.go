package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// TerminalBackendRow is one registered backend the setup menu offers after the built in ones
type TerminalBackendRow struct {
	Name        string
	DisplayName string
	Description string
}

// Registered backends outside the reserved built in names sorted by name, the rows read before a failing provider kept
func SetupTerminalBackendRows(ctx context.Context, env *Env) (rows []TerminalBackendRow) {
	defer func() {
		if r := recover(); r != nil {
			envLog(env).Debug(fmt.Sprintf("terminal backend plugin listing failed: %v", r))
		}
	}()
	discoverErr := errPluginDiscoveryUnwired
	if EnsurePluginsDiscovered != nil {
		discoverErr = EnsurePluginsDiscovered(ctx)
	}
	if discoverErr != nil {
		return nil
	}
	var providers []BackendProvider
	for _, name := range TerminalBackendNames() {
		if builtinBackends[name] {
			continue
		}
		if p := GetTerminalBackend(name); p != nil {
			providers = append(providers, p)
		}
	}
	sort.SliceStable(providers, func(i, j int) bool { return providers[i].Name() < providers[j].Name() })
	for _, p := range providers {
		rows = append(rows, TerminalBackendRow{Name: pystr.Lower(pystr.Strip(p.Name())), DisplayName: p.DisplayName(), Description: p.Description()})
	}
	return rows
}

// Docker or Podman CLI path with its runtime name, both empty when neither is installed
func SetupDockerRuntime(env *Env) (path, runtime string) {
	if path = setupFindDocker(env); path != "" {
		runtime = DockerRuntimeName(path)
	}
	return path, runtime
}

// Non blank projectId and orgId of the nearest .vercel/project.json at or above start, the working directory when start is empty
func NearestVercelProject(start string) (map[string]string, error) {
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		start = cwd
	}
	current, err := pypath.Resolve(start)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(current); err == nil && info.Mode().IsRegular() {
		current = filepath.Dir(current)
	}
	for directory := current; ; directory = filepath.Dir(directory) {
		projectFile := filepath.Join(directory, ".vercel", "project.json")
		if _, err := os.Stat(projectFile); err == nil {
			return vercelProjectIDs(projectFile)
		}
		if filepath.Dir(directory) == directory {
			return map[string]string{}, nil
		}
	}
}

// Ids a Vercel link file holds, none when it cannot be read, is not JSON or is not an object
func vercelProjectIDs(projectFile string) (map[string]string, error) {
	out := map[string]string{}
	raw, err := os.ReadFile(projectFile)
	if err != nil {
		return out, nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		return out, nil
	}
	var get func(key string) (any, bool)
	switch obj := data.(type) {
	case *pyjson.Object:
		get = obj.Get
	case map[string]any:
		get = func(key string) (any, bool) { v, held := obj[key]; return v, held }
	default:
		return out, nil
	}
	for _, key := range []string{"projectId", "orgId"} {
		if v, held := get(key); held {
			if str, isStr := v.(string); isStr && pystr.Strip(str) != "" {
				out[key] = str
			}
		}
	}
	return out, nil
}

// Command line of the batch mode connection probe, the port named only when it is not 22
func sshProbeArgv(host, user, port, key string) []string {
	argv := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5"}
	if key != "" {
		argv = append(argv, "-i", key)
	}
	if port != "" && port != "22" {
		argv = append(argv, "-p", port)
	}
	target := host
	if user != "" {
		target = user + "@" + host
	}
	return append(argv, target, "echo ok")
}

// Runs the ssh probe and prints its outcome, true when the remote echo succeeded
func TestSSHConnection(ctx context.Context, out io.Writer, host, user, port, key string) bool {
	fmt.Fprintln(out, "Testing connection...")
	code, stderr, err := setupRunSSH(ctx, sshProbeArgv(host, user, port, key))
	switch {
	case err != nil:
		fmt.Fprintf(out, "SSH connection failed: %s\n", err.Error())
	case code == 0:
		fmt.Fprintln(out, "SSH connection successful!")
		return true
	default:
		fmt.Fprintf(out, "SSH connection failed: %s\n", pystr.Strip(stderr))
	}
	fmt.Fprintln(out, "Check your SSH key and host settings.")
	return false
}

// Prints a registered backend's name and setup lines and runs its post setup hook, a failure printed instead
func RunTerminalBackendSetup(ctx context.Context, env *Env, out io.Writer, backend string) bool {
	say := func(line string) { fmt.Fprintln(out, line) }
	if err := runPluginBackendSetup(ctx, env, backend, say, say); err != nil {
		fmt.Fprintf(out, "Backend plugin setup hook failed: %s\n", err.Error())
		return false
	}
	return true
}

// Announces a registered backend, says its setup lines and runs its post setup hook, a panic returned as the error
func runPluginBackendSetup(ctx context.Context, env *Env, backend string, header, line func(string)) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	provider := GetTerminalBackend(backend)
	if provider == nil {
		return errors.New("the terminal backend plugin is not registered")
	}
	header("Terminal backend: " + provider.DisplayName())
	for _, text := range provider.SetupInstructions(env) {
		line(text)
	}
	if hook, hooked := provider.(TerminalBackendPostSetup); hooked {
		return hook.PostSetup(ctx, env)
	}
	return nil
}
