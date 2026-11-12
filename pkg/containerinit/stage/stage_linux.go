package stage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/containerinit"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
)

const (
	rebootstrapEnv      = containerinit.RebootstrapEnv
	bootstrapClientID   = "hermes-cli-vps"
	stageVerb           = containerinit.StageVerb
	dependencyMinAgeSec = 86400
)

// System command directory the image tools are linked into
var systemBinDir = "/usr/local/bin"

// Runs a bootstrap step the init delegates to the service account when argv asks for one
func DispatchStageEntry(ctx context.Context) {
	if len(os.Args) < 3 || os.Args[1] != stageVerb {
		return
	}
	os.Exit(runStage(ctx, os.Args[2], os.Args[3:]))
}

func runStage(ctx context.Context, name string, args []string) int {
	home := containerinit.DataHome()
	switch name {
	case "config-migrate":
		backend := provider.SelectedConfigBackendName(home)
		switch {
		case backend == "remote":
			fmt.Println("[stage2] ICEBATH_CONFIG_BACKEND=remote: skipping the config migration")
		case backend == "":
			fmt.Println("[stage2] Warning: could not determine the config backend; skipping the config migration")
		case isFile(filepath.Join(home, "config.yaml")):
			if config.DockerConfigMigrateMain(ctx, home) != 0 {
				fmt.Println("[stage2] Warning: config migration failed; continuing")
			}
		}
		return 0
	case "dependencies":
		root := pm.InstallRoot()
		state, err := pm.RefreshDependencies(ctx, root)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("[stage2] dependency environment:", state)
		removed, err := pm.CollectGenerations(ctx, root, dependencyMinAgeSec*time.Second)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		runtimes, err := pm.CollectRuntimeGenerations(filepath.Join(pm.InstallStateDir(ctx, root), "pm-runtime"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("[stage2] collected", len(removed)+len(runtimes), "unused dependency generations")
		return 0
	case "rebootstrap":
		authPath := ""
		if len(args) > 0 {
			authPath = args[0]
		}
		return rebootstrapMain(authPath)
	case "skills-sync":
		if err := skills.SyncSkillsMain(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "reconcile":
		return gatewaycli.ContainerBootMain(ctx, os.Stdout)
	case "image-tools":
		if err := linkImageTools(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "%s: unknown step %s\n", stageVerb, name)
	return 2
}

// Python truth value of a decoded JSON value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case *pyjson.Object:
		return x.Len() > 0
	}
	return true
}

// True when the local Nous entry needs a relogin and holds no token to recover with
func nousEntryIsTerminal(state any) bool {
	obj, ok := state.(*pyjson.Object)
	if !ok {
		return false
	}
	lastErr, _ := obj.Get("last_auth_error")
	errObj, ok := lastErr.(*pyjson.Object)
	if !ok {
		return false
	}
	if relogin, _ := errObj.Get("relogin_required"); !pyTruthy(relogin) {
		return false
	}
	access, _ := obj.Get("access_token")
	refresh, _ := obj.Get("refresh_token")
	return !pyTruthy(access) && !pyTruthy(refresh)
}

// Nous provider entry of a bootstrap seed issued to the VPS client with both tokens, nil otherwise
func extractNousFromSeed(raw string) *pyjson.Object {
	seed, err := pyjson.Loads(raw)
	if err != nil {
		return nil
	}
	obj, ok := seed.(*pyjson.Object)
	if !ok {
		return nil
	}
	providersV, _ := obj.Get("providers")
	providers, ok := providersV.(*pyjson.Object)
	if !ok {
		return nil
	}
	nousV, _ := providers.Get("nous")
	nous, ok := nousV.(*pyjson.Object)
	if !ok || nous.Len() == 0 {
		return nil
	}
	if cid, _ := nous.Get("client_id"); cid != bootstrapClientID {
		return nil
	}
	for _, k := range []string{"access_token", "refresh_token"} {
		v, _ := nous.Get(k)
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil
		}
	}
	return nous
}

// Timezone aware ISO timestamp, false when missing, malformed or naive
func parseTimestamp(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return time.Time{}, false
	}
	t, aware, err := pydatetime.ParseISOFormat(s)
	if err != nil || !aware {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// True when the seed carries a strictly newer obtained_at than the local entry
func seedIsNewer(local any, seed *pyjson.Object) bool {
	obj, ok := local.(*pyjson.Object)
	if !ok {
		return false
	}
	lv, _ := obj.Get("obtained_at")
	sv, _ := seed.Get("obtained_at")
	lt, lok := parseTimestamp(lv)
	st, sok := parseTimestamp(sv)
	return lok && sok && st.After(lt)
}

// Replaces a terminal or stale Nous entry of auth.json with the seed, returning the status word
func reseedIfTerminal(authPath, seedRaw string) (string, error) {
	if seedRaw == "" {
		return "no_seed", nil
	}
	seed := extractNousFromSeed(seedRaw)
	if seed == nil {
		return "bad_seed", nil
	}
	if _, err := os.Stat(authPath); err != nil {
		return "no_auth_file", nil
	}
	raw, err := os.ReadFile(authPath)
	if err != nil {
		return "auth_unreadable", nil
	}
	parsed, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	if err != nil {
		return "auth_unreadable", nil
	}
	store, ok := parsed.(*pyjson.Object)
	if !ok {
		return "auth_unreadable", nil
	}
	providersV, _ := store.Get("providers")
	providers, ok := providersV.(*pyjson.Object)
	if !ok {
		providers = pyjson.NewObject()
		store.Set("providers", providers)
	}
	local, _ := providers.Get("nous")
	terminal := nousEntryIsTerminal(local)
	if !terminal && !seedIsNewer(local, seed) {
		return "not_terminal", nil
	}
	providers.Set("nous", seed)
	text, err := pyjson.Dumps(store)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(authPath)
	tmp, err := os.CreateTemp(dir, filepath.Base(authPath)+".rebootstrap.*.tmp")
	if err != nil {
		return "", err
	}
	_, werr := tmp.WriteString(text)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), authPath)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return "", werr
	}
	if terminal {
		return "reseeded", nil
	}
	return "reseeded_newer", nil
}

// Re-seeds the Nous session at boot, an error printed and ignored so the boot never fails here
func rebootstrapMain(authPath string) int {
	if authPath == "" {
		if h := os.Getenv("ICEBATH_HOME"); h != "" {
			authPath = filepath.Join(h, "auth.json")
		} else {
			authPath = "auth.json"
		}
	}
	result, err := reseedIfTerminal(authPath, os.Getenv(rebootstrapEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rebootstrap] error (ignored): %s\n", pyrepr.ExceptionRepr(err))
		return 0
	}
	switch result {
	case "reseeded":
		fmt.Println("[rebootstrap] Nous bootstrap session was terminal; re-seeded auth.json from " + rebootstrapEnv)
	case "reseeded_newer":
		fmt.Println("[rebootstrap] Applied newer orchestrator-issued Nous bootstrap session from " + rebootstrapEnv)
	default:
		fmt.Printf("[rebootstrap] no-op (%s)\n", result)
	}
	return 0
}

// Links the pm installed media and search tools onto PATH and pins Chromium for the browser runtime
func linkImageTools(ctx context.Context) error {
	binary := func(name string) (string, error) {
		installed, err := pm.Installed(ctx, name, false)
		if err != nil {
			return "", err
		}
		if installed == nil || installed.Binary == "" {
			return "", fmt.Errorf("pm package %s is not installed", name)
		}
		return installed.Binary, nil
	}
	ffmpeg, err := binary("ffmpeg")
	if err != nil {
		return err
	}
	rg, err := binary("ripgrep")
	if err != nil {
		return err
	}
	chromium, err := binary("chromium")
	if err != nil {
		return err
	}
	links := [][2]string{{"ffmpeg", ffmpeg}, {"ffprobe", filepath.Join(filepath.Dir(ffmpeg), "ffprobe")}, {"rg", rg}}
	for _, l := range links {
		dest := filepath.Join(systemBinDir, l[0])
		os.Remove(dest)
		if err := os.Symlink(l[1], dest); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(containerinit.ChromiumPinPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(containerinit.ChromiumPinPath(), []byte(chromium), 0o644)
}

// True when p names a regular file
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
