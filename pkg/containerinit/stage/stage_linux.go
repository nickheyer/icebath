package stage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/containerinit"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
)

const (
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
		fmt.Println("[stage2] collected", len(removed), "unused dependency generations")
		return 0
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

// Links the pm installed interpreters, package managers, media and search tools onto PATH and pins Chromium
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
	var links [][2]string
	for _, l := range [][2]string{{"python3", "python"}, {"node", "node"}, {"npm", "npm"}, {"ffmpeg", "ffmpeg"}, {"rg", "ripgrep"}} {
		target, err := binary(l[1])
		if err != nil {
			return err
		}
		links = append(links, [2]string{l[0], target})
	}
	links = append(links, [2]string{"ffprobe", filepath.Join(filepath.Dir(links[3][1]), "ffprobe")})
	npmPath := pm.EnvFor([]string{"npm"}, map[string]string{})["PATH"]
	npx, ok := pyshutil.Which("npx", &npmPath)
	if !ok {
		return fmt.Errorf("npx is not on the pm npm PATH %s", npmPath)
	}
	links = append(links, [2]string{"npx", npx})
	chromium, err := binary("chromium")
	if err != nil {
		return err
	}
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
