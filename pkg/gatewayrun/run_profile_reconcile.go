package gatewayrun

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/profiles"
)

const (
	profileRescanInterval  = 30 * time.Second
	ownGatewayProbeTimeout = 5 * time.Second
)

// Files besides config.yaml whose change rebuilds a served profile's adapters
var profileSignatureFiles = []string{".env"}

// Content version of path hashed every time, nil when it cannot be read
func statSignature(path string) []any {
	version, err := config.FileCacheVersion(path, true)
	if err != nil {
		if pypath.IsOSError(err) {
			return nil
		}
		panic(err)
	}
	return version
}

// Change detector of a served profile's config layer and .env, compared on a later reconcile tick
func profileServeSignature(ctx context.Context, profileHome string) ([]any, error) {
	cfg := filepath.Join(profileHome, "config.yaml")
	fileTooling, err := config.SupportsFileTooling(ctx)
	if err != nil {
		return nil, err
	}
	var first any
	if fileTooling {
		if sig := statSignature(cfg); sig != nil {
			first = sig
		}
	} else {
		version, err := config.ConfigVersionOrNone(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if version != nil {
			first = version
		}
	}
	sig := []any{first}
	for _, name := range profileSignatureFiles {
		version, err := config.FileCacheVersion(filepath.Join(profileHome, name), true)
		switch {
		case err == nil:
			sig = append(sig, version)
		case pypath.IsOSError(err):
			sig = append(sig, nil)
		default:
			return nil, err
		}
	}
	return sig, nil
}

// Whether two serve signatures are equal, an unset one equal to nothing
func signaturesEqual(a, b []any) bool {
	return a != nil && b != nil && reflect.DeepEqual(a, b)
}

// Lock serialising served profile reconciles, created on first use
func (r *GatewayRunner) reconcileLock() *sync.Mutex {
	r.profileReconcileLockOnce.Do(func() {
		if r.profileReconcileLock == nil {
			r.profileReconcileLock = &sync.Mutex{}
		}
	})
	return r.profileReconcileLock
}

// Profiles this multiplexer serves now, the active profile first then the rest sorted
func (r *GatewayRunner) servedProfileNames() []string {
	active := r.primaryProfileName
	if active == "" {
		active = "default"
	}
	var names []string
	empty := r.servedProfileHomes == nil || r.servedProfileHomes.Len() == 0
	if empty || r.servedProfileHomes.Has(active) {
		names = append(names, active)
	}
	var rest []string
	if r.servedProfileHomes != nil {
		for _, name := range r.servedProfileHomes.Keys() {
			if name != active {
				rest = append(rest, name)
			}
		}
	}
	slices.Sort(rest)
	return append(names, rest...)
}

// Remembers the served set and each home's serve signature, keeping signatures already taken
func (r *GatewayRunner) noteServedProfiles(ctx context.Context, profileHomes []profiles.ServedProfile) error {
	homes := pydict.New[string, string]()
	for _, p := range profileHomes {
		homes.Set(p.Name, p.Home)
	}
	r.servedProfileHomes = homes
	previous := r.servedProfileSignatures
	if previous == nil {
		previous = map[string][]any{}
	}
	signatures := map[string][]any{}
	for name, profileHome := range homes.All() {
		if sig := previous[name]; len(sig) > 0 {
			signatures[name] = sig
			continue
		}
		sig, err := profileServeSignature(ctx, profileHome)
		if err != nil {
			return err
		}
		signatures[name] = sig
	}
	r.servedProfileSignatures = signatures
	return nil
}
