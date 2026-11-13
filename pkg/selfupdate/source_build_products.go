package selfupdate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Steps of an update's product builds, the hermes module functions its tests replace
var productSteps = struct {
	featureDeps     func(ctx context.Context, projectRoot string) error
	publishStage    func(message string)
	memoryMigrate   func(ctx context.Context, say func(string)) ([]string, error)
	leftCoreMigrate func(ctx context.Context, say func(string)) ([]string, error)
	dependencyHomes func(ctx context.Context) ([]string, error)
	frontends       func(projectRoot string) []string
	buildEnv        func(ctx context.Context, baseEnv map[string]string, explicit, verify bool) (map[string]string, error)
	prepareDeps     func(ctx context.Context, projectRoot string, workspaces []string, env map[string]string, explicit bool) error
	buildWeb        func(ctx context.Context, projectRoot string, env map[string]string, icons string) error
	buildDesktop    func(ctx context.Context, desktopDir string, sourceMode bool, npm string, env map[string]string, icons string) (string, error)
	refreshApps     func(ctx context.Context, desktopDir string)
	desktopLock     func(ctx context.Context, projectRoot string, wait bool) (*DesktopBuildLock, error)
}{
	featureDeps:     pm.InstallConfiguredFeaturesMissingDeps,
	publishStage:    PublishStage,
	memoryMigrate:   memory.MigrateAllMemoryHomes,
	leftCoreMigrate: provider.MigrateAllLeftCoreHomes,
	dependencyHomes: pm.DependencyHomes,
	frontends:       sourceFrontends,
	buildEnv:        SourceBuildEnv,
	prepareDeps:     PrepareSourceDependencies,
	buildWeb:        BuildSourceWeb,
	refreshApps:     refreshInstalledDesktopApps,
	desktopLock: func(ctx context.Context, projectRoot string, wait bool) (*DesktopBuildLock, error) {
		lock := NewDesktopBuildLock(projectRoot)
		_, err := lock.Acquire(ctx, wait)
		return lock, err
	},
}

// Binds the desktop build step, which itself publishes its packaging stage through these steps
func init() { productSteps.buildDesktop = BuildPreparedDesktop }

// Prepares the selected workspace union once and attempts every independent product, failures raised together
func BuildUpdateProducts(ctx context.Context, projectRoot string, desktop bool) error {
	out := desktopOut
	var failures []productFailure
	productSteps.publishStage("Building the apps")
	attempt := func(name string, step func() error) bool {
		if err := step(); err != nil {
			fmt.Fprintf(out, "  ⚠ %s failed: %s\n", name, failureText(err))
			failures = append(failures, productFailure{name, err})
			return false
		}
		return true
	}
	attempt("feature dependencies", func() error { return productSteps.featureDeps(ctx, projectRoot) })
	frontends := productSteps.frontends(projectRoot)
	if len(frontends) > 0 {
		desktopBuilt := false
		env := map[string]string{}
		workspaces := append([]string{}, frontends...)
		if desktop {
			workspaces = append(workspaces, "apps/desktop")
		}
		productSteps.publishStage("Updating Node dependencies")
		nodeDependencies := func(selected []string) error {
			acquired, err := productSteps.buildEnv(ctx, nil, true, false)
			if err != nil {
				return err
			}
			for k, v := range acquired {
				env[k] = v
			}
			return productSteps.prepareDeps(ctx, projectRoot, selected, env, true)
		}
		prepared := attempt("Node dependencies", func() error { return nodeDependencies(workspaces) })
		desktopSkipped := false
		if !prepared && desktop && len(env) > 0 {
			fmt.Fprintln(out, "  → Retrying Node dependencies without the desktop app")
			failed := failures[len(failures)-1]
			failures = failures[:len(failures)-1]
			prepared = attempt("Node dependencies", func() error { return nodeDependencies(frontends) })
			desktopSkipped = prepared
			if desktopSkipped {
				failures = append(failures, productFailure{"desktop dependencies", failed.Err})
			}
		}
		desktopReason := ""
		if len(failures) > 0 {
			desktopReason = failures[len(failures)-1].Name
		}
		if desktop && (desktopSkipped || headlessLinux()) {
			fmt.Fprintln(out, "  ⓘ If this host never runs the desktop app, `icebath uninstall --gui` removes it "+
				"and future updates stop building it.")
		}
		if prepared {
			if slices.Contains(frontends, "web") {
				if sourceProductCurrent(ctx, projectRoot, "web", WebDistDir(projectRoot)) {
					fmt.Fprintln(out, "  ✓ Web UI is up to date")
				} else {
					productSteps.publishStage("Building the web UI")
					attempt("web UI build", func() error { return productSteps.buildWeb(ctx, projectRoot, env, "") })
				}
			}
			if desktop && !desktopSkipped {
				desktopBuilt = attempt("desktop app build", func() error { return buildDesktopProduct(ctx, projectRoot, env) })
				desktopReason = ""
				if !desktopBuilt {
					desktopReason = failures[len(failures)-1].Name
				}
			}
		}
		if desktop && !desktopBuilt {
			fmt.Fprintf(out, "  Desktop app build owed: %s failed\n", desktopReason)
		}
	}
	if info, err := os.Stat(filepath.Join(projectRoot, "apps", "desktop", "vite.config.ts")); err == nil && info.Mode().IsRegular() {
		attempt("Webapp build", func() error { return rebuildServedWebapp(ctx, projectRoot) })
	}
	say := func(message string) { fmt.Fprintln(out, message) }
	if _, err := productSteps.memoryMigrate(ctx, say); err != nil {
		fmt.Fprintf(out, "  ⚠ Memory provider migration skipped: %v\n", err)
	}
	if _, err := productSteps.leftCoreMigrate(ctx, say); err != nil {
		fmt.Fprintf(out, "  ⚠ Plugin migration skipped: %v\n", err)
	}
	if len(failures) > 0 {
		return &ProductBuildError{Failures: failures}
	}
	return nil
}

// Rebuilds the webapp bundle a home sharing this install serves, marking it stale when the build fails
func rebuildServedWebapp(ctx context.Context, projectRoot string) error {
	homes, err := productSteps.dependencyHomes(ctx)
	if err != nil {
		return err
	}
	served := false
	for _, h := range homes {
		if gateway.WebappSettingsOf(ctx, h).Enabled {
			served = true
			break
		}
	}
	if !served {
		return nil
	}
	productSteps.publishStage("Building the Webapp")
	if _, err := PrepareWebappRenderer(ctx, projectRoot, false, false, true); err != nil {
		marked, markErr := MarkWebappStale(projectRoot)
		if markErr != nil {
			fmt.Fprintf(desktopOut, "  ⚠ Could not mark the Webapp out of date: %s\n", desktopErrText(markErr))
		} else if marked {
			fmt.Fprintln(desktopOut, "  ⓘ The Webapp stays unavailable until `icebath webapp` rebuilds it.")
		}
		return err
	}
	return nil
}

// Reuses a packaged desktop app naming HEAD or unchanged inputs, else builds it under the desktop build lock
func buildDesktopProduct(ctx context.Context, projectRoot string, env map[string]string) error {
	desktopDir := filepath.Join(projectRoot, "apps", "desktop")
	if packagedDesktopCurrentForHead(ctx, desktopDir, projectRoot) {
		fmt.Fprintln(desktopOut, "  ✓ Desktop app is up to date")
	} else if builtFrom := packagedDesktopReusable(ctx, desktopDir, projectRoot, env); builtFrom != "" {
		fmt.Fprintf(desktopOut, "  ✓ Desktop app is up to date: no desktop build input changed since %s\n", builtFrom[:min(12, len(builtFrom))])
	} else {
		productSteps.publishStage("Building the desktop app")
		lock, err := productSteps.desktopLock(ctx, projectRoot, true)
		if err != nil {
			return err
		}
		err = func() error {
			defer lock.Release(ctx)
			path, ok := env["PATH"]
			if !ok {
				return sourceKeyError{"PATH"}
			}
			npm, _ := pyshutil.Which("npm", &path)
			_, err := productSteps.buildDesktop(ctx, desktopDir, false, npm, env, projectRoot)
			return err
		}()
		if err != nil {
			return err
		}
	}
	productSteps.refreshApps(ctx, desktopDir)
	return nil
}
