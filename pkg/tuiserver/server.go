package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Stand in terminal cwd values resolved to the home dir at runtime, never an explicit workspace
var cwdPlaceholders = map[string]bool{".": true, "auto": true, "cwd": true}

// CLI banner facts the session info reports, assigned by the command wiring from the app package
var Banner struct {
	ReleaseDate     string
	AvailableSkills func(ctx context.Context) *pyjson.Object
	UpdateResult    func(timeout time.Duration) *int64
}

// Launch home captured on first use, the launch state db handle and the homes served besides the launch one
var (
	icebathHome         string
	icebathHomeAtImport string
	homeImportOnce      sync.Once
	dbMu                sync.Mutex
	sharedDB            *state.DB
	dbError             error
	servedProfileMu     sync.Mutex
	servedProfileHomes  = map[string]bool{}
)

// Home captured on the first read after the profile override, the value tui_gateway.server takes at import
func importedHome() string {
	homeImportOnce.Do(func() {
		if icebathHome == "" {
			icebathHome = home.Process()
			icebathHomeAtImport = icebathHome
		}
	})
	return icebathHome
}

// Launch profile home at call time, the swapped copy when a harness changed it else the live process home
func launchHome() string {
	if importedHome() != icebathHomeAtImport {
		return icebathHome
	}
	return home.Process()
}

// state.db of the launch profile
func launchStateDBPath() string { return filepath.Join(launchHome(), "state.db") }

// Shared launch session store opened on first use, else the open failure described for the user
func getDB(ctx context.Context) (*state.DB, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if sharedDB == nil {
		path := launchStateDBPath()
		db, err := state.Acquire(ctx, path)
		if err != nil {
			dbError = err
			return nil, storeUnavailable(ctx, path, err)
		}
		sharedDB, dbError = db, nil
	}
	return sharedDB, nil
}

// Session store that would not open, shared with the messaging gateway so both name the cause alike
type StoreUnavailableError = state.StoreUnavailableError

// Open failure of the store at path described with the storage failure gloss, details and remedy
func storeUnavailable(ctx context.Context, path string, cause error) *StoreUnavailableError {
	return state.NewStoreUnavailableError(ctx, path, cause)
}

// Effective user config of the active profile without the defaults merge
func loadCfg(ctx context.Context) map[string]any { return tuihost.LoadCfg(ctx) }

// display section of the behavioral config, empty when absent or malformed
func displayCfg(ctx context.Context) map[string]any {
	display, _ := loadCfg(ctx)["display"].(map[string]any)
	if display == nil {
		return map[string]any{}
	}
	return display
}

// Explicit profile param naming no live profile on this host, answered as JSON RPC 4064
type ProfileUnavailableError struct{ Msg string }

func (e *ProfileUnavailableError) Error() string { return e.Msg }

// Python exception class name the error stands for
func (e *ProfileUnavailableError) PyTypeName() string { return "ProfileUnavailableError" }

// Older payload basenames of the default home mapped to default unless a real profile carries that name
func canonicalProfileRequest(name string) string {
	folded := strings.ToLower(name)
	if folded == ".icebath" || folded == "icebath" {
		normalized, err := profiles.NormalizeProfileName(name)
		if err != nil || !isDir(filepath.Join(profiles.ProfilesRoot(), normalized)) {
			return "default"
		}
	}
	return name
}

// Named profile home on this host, empty for the launch profile, a ProfileUnavailableError for an unknown one
func profileHome(profile string) (string, error) {
	name := canonicalProfileRequest(strings.TrimSpace(profile))
	if name == "" {
		return "", nil
	}
	dir, err := profiles.GetProfileDir(name)
	if err != nil || dir == "" || !isDir(dir) {
		return "", &ProfileUnavailableError{fmt.Sprintf("Profile '%s' does not exist.", name)}
	}
	if resolveLax(dir) == resolveLax(importedHome()) {
		return "", nil
	}
	servedProfileMu.Lock()
	defer servedProfileMu.Unlock()
	if !servedProfileHomes[dir] {
		provider.ActivateMultiProfileHosting()
	}
	servedProfileHomes[dir] = true
	return dir, nil
}

// Working folder setting of a profile home as a host directory, empty when unset, a stand in or missing on this host
func profileConfiguredCwd(ctx context.Context, profileHome string) string {
	if profileHome == "" {
		return ""
	}
	raw := strings.TrimSpace(settings.WorkingFolder.Get(home.WithOverride(ctx, profileHome)))
	if raw == "" || cwdPlaceholders[raw] {
		return ""
	}
	resolved, err := pypath.Abspath(pypath.Expanduser(raw))
	if err != nil || !isDir(resolved) {
		return ""
	}
	return resolved
}

// Launch profile terminal cwd read by path, never through the active home override
func launchConfiguredCwd(ctx context.Context) string {
	return profileConfiguredCwd(ctx, importedHome())
}

// Active profile name, default when none is active or the lookup fails
func currentProfileName(ctx context.Context) (name string) {
	defer func() {
		if recover() != nil {
			name = "default"
		}
	}()
	if active := profiles.GetActiveProfileName(ctx); active != "" {
		return active
	}
	return "default"
}

// Named project owning the cwd in the profile projects db, nil when none or on failure
func projectInfoForCwd(ctx context.Context, cwd string) any {
	if strings.TrimSpace(cwd) == "" {
		return nil
	}
	db, err := tools.OpenProjectsDB(ctx, &tools.Env{Home: home.Dir(ctx)})
	if err != nil {
		pkgLog().Debug("failed to resolve project for cwd", "error", err)
		return nil
	}
	defer func() { _ = db.Close() }()
	project, err := db.ProjectForPath(cwd, false)
	if err != nil {
		pkgLog().Debug("failed to resolve project for cwd", "error", err)
		return nil
	}
	if project == nil {
		return nil
	}
	var primary any
	if project.PrimaryPath != nil {
		primary = *project.PrimaryPath
	}
	return map[string]any{"id": project.ID, "slug": project.Slug, "name": project.Name, "primary_path": primary}
}

// Runs a body and swallows any panic it raises
func suppress(body func()) {
	defer func() { _ = recover() }()
	body()
}

// Python truthiness of a slot value
func truthy(v any) bool { return pyrepr.Truthy(v) }

// str of a value with None and falsy values read as the empty string
func strOr(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Whether a path names an existing directory
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Lenient pathlib resolve, the absolute path when a component cannot be resolved
func resolveLax(p string) string {
	if resolved, err := pypath.Resolve(p); err == nil {
		return resolved
	}
	abs, _ := filepath.Abs(p)
	return abs
}

// Session storage unavailable error for cause with the user facing gloss, action and raw cause details
func storeUnavailableReply(ctx context.Context, rid any, code int, cause error) map[string]any {
	failure := state.DescribeStorageFailure(ctx, cause)
	return rpcErr(rid, code, fmt.Sprintf("Session storage is unavailable: %s. %s", failure.Gloss, failure.Action),
		map[string]any{"code": failure.Code, "cause": failure.Cause, "details": state.StorageFailureDetails(cause, 200)})
}

// Underlying open failure of a store error, the error itself for any other failure
func storeCause(err error) error {
	var unavailable *StoreUnavailableError
	if errors.As(err, &unavailable) {
		return unavailable.Cause
	}
	return err
}

// Storage error reply for a store that would not open or a query it failed, naming the cause
func storeErrorReply(ctx context.Context, rid any, code int, err error) map[string]any {
	return storeUnavailableReply(ctx, rid, code, storeCause(err))
}

// Storage error reply for a store that would not open, any other failure raised to the dispatcher
func storeFailure(ctx context.Context, rid any, code int, err error) map[string]any {
	var unavailable *StoreUnavailableError
	if errors.As(err, &unavailable) {
		return storeUnavailableReply(ctx, rid, code, unavailable.Cause)
	}
	panicIf(err)
	return nil
}

// Wraps a handler in the runtime scope of the params profile, else the ticket's
func profileScoped(handler Handler) Handler {
	return func(ctx context.Context, rid any, params map[string]any) map[string]any {
		scopeHome := connectionProfileHome(ctx)
		if strings.TrimSpace(strOr(params["profile"])) != "" {
			h, err := profileHome(strOr(params["profile"]))
			if err != nil {
				panic(err)
			}
			scopeHome = h
		}
		return handler(profileRuntimeScope(ctx, scopeHome, true), rid, params)
	}
}

// Whether a raised failure is a config backend outage, the SystemExit no handler catches
func configOutage(failure any) bool {
	err, isError := failure.(error)
	if !isError {
		return false
	}
	var backendErr *config.ConfigBackendUnavailable
	var sourceErr *provider.ConfigBackendUnavailableError
	return errors.As(err, &backendErr) || errors.As(err, &sourceErr)
}

// Python str of a value or empty string for a falsy one
func relayString(v any) string {
	if !truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}
