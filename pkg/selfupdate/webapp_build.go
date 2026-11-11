package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
)

const (
	webappLockName        = ".web_ui_build.lock"
	webappLockWait        = 30 * time.Minute
	WebappDistEnv         = "ICEBATH_WEBAPP_DIST"
	webappStaleMarkerText = "icebath update could not rebuild this bundle\n"
)

// Receipt reader, build env and compiler of the renderer, the source_build functions hermes tests replace
var webappSteps = struct {
	buildEnv func(ctx context.Context, explicit bool) (map[string]string, error)
	build    func(ctx context.Context, projectRoot string, env map[string]string, explicit bool) error
}{
	buildEnv: func(ctx context.Context, explicit bool) (map[string]string, error) {
		return SourceBuildEnv(ctx, nil, explicit, true)
	},
	build: buildSourceWebapp,
}

// Browser hosted desktop renderer that could not be prepared
type WebappBuildError struct{ Msg string }

func (e *WebappBuildError) Error() string         { return e.Msg }
func (e *WebappBuildError) ExceptionName() string { return "WebappBuildError" }

// Bundle the packaged dist env names, empty when icebath builds its own
func packagedWebappDist() string {
	value := os.Getenv(WebappDistEnv)
	if value == "" {
		return ""
	}
	return pypath.Expanduser(value)
}

// Directory the webapp renderer of a checkout is served from
func WebappDistDir(projectRoot string) string {
	if packaged := packagedWebappDist(); packaged != "" {
		return packaged
	}
	return filepath.Join(projectRoot, "apps", "desktop", "dist-webapp")
}

// Marks the built bundle out of date after a failed rebuild, false without a built or with a packaged bundle
func MarkWebappStale(projectRoot string) (bool, error) {
	dist := WebappDistDir(projectRoot)
	if info, err := os.Stat(filepath.Join(dist, "index.html")); packagedWebappDist() != "" || err != nil || !info.Mode().IsRegular() {
		return false, nil
	}
	if err := os.WriteFile(filepath.Join(dist, gateway.WebappStaleMarker), []byte(webappStaleMarkerText), 0o666); err != nil {
		return false, err
	}
	return true, nil
}

// True when the bundle in dist carries the out of date marker
func WebappBundleStale(dist string) bool { return gateway.WebappBundleStale(dist) }

// Removes the out of date marker of a bundle the build just proved current
func clearWebappStaleMark(dist string) error {
	if err := os.Remove(filepath.Join(dist, gateway.WebappStaleMarker)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &WebappBuildError{fmt.Sprintf("Could not mark the Webapp renderer at %s current: %s", dist, pypath.OSErrorText(err))}
	}
	return nil
}

// Holds the exclusive renderer build lock of the checkout while fn runs, waiting behind another build
func withWebappBuildLock(path string, fn func() (string, error)) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return "", &WebappBuildError{fmt.Sprintf("Could not open Webapp build lock %s: %s", path, pypath.OSErrorText(err))}
	}
	handle, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return "", &WebappBuildError{fmt.Sprintf("Could not open Webapp build lock %s: %s", path, pypath.OSErrorText(err))}
	}
	defer func() {
		pm.UnlockFD(handle)
		handle.Close()
	}()
	held, err := pm.LockFD(handle, false, 0)
	if err == nil && !held {
		fmt.Fprintln(desktopOut, "→ Another Icebath Webapp build is running; waiting for it...")
		held, err = pm.LockFD(handle, true, webappLockWait)
		if err == nil && !held {
			return "", &WebappBuildError{fmt.Sprintf("Timed out waiting for another Webapp build (%s)", path)}
		}
	}
	if err != nil {
		return "", &WebappBuildError{fmt.Sprintf("Could not lock Webapp build %s: %s", path, pypath.OSErrorText(err))}
	}
	return fn()
}

// Verified browser renderer, reusing a current bundle and serializing concurrent builds
var PrepareWebappRenderer = func(ctx context.Context, projectRoot string, force, skipBuild, explicit bool) (string, error) {
	if resolved, err := pypath.Resolve(projectRoot); err == nil {
		projectRoot = resolved
	}
	dist := WebappDistDir(projectRoot)
	indexBuilt := func() bool {
		info, err := os.Stat(filepath.Join(dist, "index.html"))
		return err == nil && info.Mode().IsRegular()
	}
	if packagedWebappDist() != "" {
		if !indexBuilt() {
			return "", &WebappBuildError{fmt.Sprintf("%s is set but no Webapp renderer exists at %s", WebappDistEnv, dist)}
		}
		if force {
			return "", &WebappBuildError{fmt.Sprintf("The Webapp renderer at %s comes from %s and cannot be rebuilt here", dist, WebappDistEnv)}
		}
		fmt.Fprintf(desktopOut, "→ Using Icebath Webapp renderer from %s: %s\n", WebappDistEnv, dist)
		return dist, nil
	}
	if skipBuild {
		if !indexBuilt() {
			return "", &WebappBuildError{fmt.Sprintf("--skip-build was passed but no Webapp renderer exists at %s", dist)}
		}
		fmt.Fprintf(desktopOut, "→ Reusing Icebath Webapp renderer at %s (--skip-build)\n", dist)
		return dist, nil
	}
	if !force && !WebappBundleStale(dist) && sourceProductCurrent(ctx, projectRoot, "webapp", dist) {
		fmt.Fprintf(desktopOut, "✓ Icebath Webapp renderer is up to date: %s\n", dist)
		return dist, nil
	}
	return withWebappBuildLock(filepath.Join(projectRoot, webappLockName), func() (string, error) {
		if !force && sourceProductCurrent(ctx, projectRoot, "webapp", dist) {
			if err := clearWebappStaleMark(dist); err != nil {
				return "", err
			}
			fmt.Fprintf(desktopOut, "✓ Icebath Webapp renderer is up to date: %s\n", dist)
			return dist, nil
		}
		env, err := webappSteps.buildEnv(ctx, explicit)
		if err == nil {
			err = webappSteps.build(ctx, projectRoot, env, explicit)
		}
		if err != nil {
			return "", &WebappBuildError{fmt.Sprintf("Browser-hosted Desktop build failed: %s", webappBuildFailureText(err))}
		}
		if err := clearWebappStaleMark(dist); err != nil {
			return "", err
		}
		fmt.Fprintf(desktopOut, "✓ Icebath Webapp renderer built: %s\n", dist)
		return dist, nil
	})
}

// Python str of a failed renderer build step
func webappBuildFailureText(err error) string {
	var called *pm.CalledProcessError
	if errors.As(err, &called) {
		return called.Error()
	}
	if pypath.IsOSError(err) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}
