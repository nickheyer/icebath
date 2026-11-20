package manage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// MCP server a create writes into the new profile, one of URL or Command, the bearer token nil when none was given
type MCPServerCreate struct {
	Name        string
	URL         string
	Command     string
	Args        []string
	Env         map[string]string
	Auth        string
	BearerToken *string
}

// Everything a surface can ask of a create, the zero value a fresh profile with its alias, the launch keys and model, and shared logins
type ProfileCreateRequest struct {
	Name          string
	CloneFrom     *string
	CloneAll      bool
	CloneConfig   bool
	CloneChannels bool
	NoSkills      bool
	SyncImports   bool
	Description   *string
	Soul          *string
	// Skips the wrapper script in the user's bin directory
	NoAlias bool
	// Lets bundled skill seeding print its own failures instead of handing them back as warnings
	Verbose bool
	// Copies no keys, logins, voice sections or model from the launch profile
	NoCredentials bool
	// Copies the launch logins once instead of reading the root's, which a named launch profile always does
	IsolatedAuth bool
	// Explicit model pin, applied only with its provider
	Model    string
	Provider string
	// Home whose catalog vouches for the pin, the new profile when empty
	ModelValidateIn string
	MCPServers      []MCPServerCreate
	// Installed skills to keep, every other one disabled when any is named
	KeepSkills []string
}

// Reports whether the request names no source profile, so the new profile gets bundled skills instead of a copy
func (r ProfileCreateRequest) Fresh() bool {
	return !((r.CloneFrom != nil && *r.CloneFrom != "") || r.CloneAll || r.CloneConfig)
}

// What a create did, Warnings naming each step that failed outright
type ProvisionResult struct {
	Path string
	// Result of the bundled skill sync, nil when skipped or failed
	SkillsSeeded   map[string]any
	AliasPath      string
	AliasCollision string
	Mirrored       tuigateway.ProfileMirrored
	ModelSet       bool
	ModelError     string
	MCPWritten     int
	SkillsDisabled int
	Warnings       []string
}

// One create lock and how many creates wait on or hold it
type createLock struct {
	mu    sync.Mutex
	users int
}

// Create locks by canonical profile name, an entry living only while a create uses it
var createLocks = struct {
	sync.Mutex
	held map[string]*createLock
}{held: map[string]*createLock{}}

// Takes the create lock of a profile name so two creates of one name in this process never share a staging dir, returning its release
func lockCreate(name string) (release func()) {
	key, err := profiles.NormalizeProfileName(name)
	if err != nil {
		key = ""
	}
	createLocks.Lock()
	lock := createLocks.held[key]
	if lock == nil {
		lock = &createLock{}
		createLocks.held[key] = lock
	}
	lock.users++
	createLocks.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		createLocks.Lock()
		if lock.users--; lock.users == 0 {
			delete(createLocks.held, key)
		}
		createLocks.Unlock()
	}
}

// Reports whether a failure is the config backend going away, which no best effort step may swallow
func configOutage(err error) bool {
	var backend *config.ConfigBackendUnavailable
	var source *provider.ConfigBackendUnavailableError
	return errors.As(err, &backend) || errors.As(err, &source)
}

// Runs a best effort step, a raised or returned failure coming back as the error
func attempt(fn func() error) (err error) {
	defer func() {
		if failure := recover(); failure != nil {
			if raised, isError := failure.(error); isError {
				err = raised
				return
			}
			err = fmt.Errorf("%v", failure)
		}
	}()
	return fn()
}

// Creates the profile under its name lock, the core create alone
func createLocked(ctx context.Context, req ProfileCreateRequest) (string, error) {
	defer lockCreate(req.Name)()
	return CreateProfile(ctx, req.Name, CreateOptions{
		CloneFrom: req.CloneFrom, CloneAll: req.CloneAll, CloneConfig: req.CloneConfig, NoSkills: req.NoSkills,
		Description: req.Description, CloneChannels: req.CloneChannels, SyncImports: req.SyncImports, Soul: req.Soul,
		NoSeedModel: req.NoCredentials,
	})
}

// Creates a profile and applies every requested step in order, only the core create and a config outage failing it
func ProvisionProfile(ctx context.Context, req ProfileCreateRequest) (*ProvisionResult, error) {
	path, err := createLocked(ctx, req)
	if err != nil {
		return nil, err
	}
	result := &ProvisionResult{Path: path, Warnings: []string{}}
	var outage error
	step := func(label string, fn func() error) bool {
		if outage != nil {
			return false
		}
		err := attempt(fn)
		if err == nil {
			return true
		}
		if configOutage(err) {
			outage = err
			return false
		}
		pkgLog().Warn(fmt.Sprintf("profile %s: %s failed", req.Name, label), "error", err)
		result.Warnings = append(result.Warnings, label+": "+CreateErrorText(err))
		return false
	}
	if req.Fresh() {
		var problems []string
		step("bundled skills", func() error {
			result.SkillsSeeded = seedProfileSkills(ctx, path, !req.Verbose, &problems)
			return nil
		})
		if !req.Verbose {
			for _, line := range problems {
				if text := strings.TrimSpace(line); text != "" {
					result.Warnings = append(result.Warnings, strings.TrimPrefix(text, "⚠ "))
				}
			}
		}
	}
	if !req.NoAlias {
		step("alias wrapper", func() error {
			wrapper, collision, err := WriteAlias(req.Name)
			result.AliasPath, result.AliasCollision = wrapper, collision
			return err
		})
	}
	if !req.NoCredentials {
		var keptRoot []string
		step("launch credentials", func() error {
			keptRoot = MirrorLaunchCredentials(ctx, path, &result.Mirrored, !req.IsolatedAuth, req.CloneChannels)
			return nil
		})
		if len(keptRoot) > 0 && provider.NewStore(home.Dir(ctx)).GlobalPath() != "" {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"subscription logins not carried over (%s): the profile uses the default "+
					"profile's; run `icebath -p %s auth add <provider>` to sign it in separately", strings.Join(keptRoot, ", "), req.Name))
		}
	}
	switch {
	case req.Model != "" && req.Provider != "":
		step("model", func() error {
			set, err := pinModel(ctx, path, req, result)
			result.ModelSet = set
			return err
		})
	case !req.NoCredentials:
		step("model inherit", func() error {
			inherited, err := InheritLaunchModel(ctx, path)
			result.Mirrored.ModelInherited = inherited
			return err
		})
	}
	if len(req.MCPServers) > 0 {
		step("MCP servers", func() error {
			written, err := WriteProfileMCPServers(ctx, path, req.MCPServers)
			result.MCPWritten = written
			return err
		})
	}
	if len(req.KeepSkills) > 0 {
		step("skill selection", func() error {
			disabled, err := DisableUnselectedSkills(ctx, path, req.KeepSkills)
			result.SkillsDisabled = disabled
			return err
		})
	}
	if outage != nil {
		return result, outage
	}
	if !result.Mirrored.Auth.Done {
		result.Mirrored.Auth = tuigateway.MirroredValue{Shared: !pathExists(filepath.Join(path, "auth.json"))}
	}
	return result, nil
}

// Wrapper script of a profile in the user's bin directory, the collision text instead when the name is taken
func WriteAlias(name string) (wrapper, collision string, err error) {
	collision, err = profiles.CheckAliasCollision(name)
	if err != nil || collision != "" {
		return "", collision, err
	}
	wrapper, err = profiles.CreateWrapperScript(name, "")
	return wrapper, "", err
}
