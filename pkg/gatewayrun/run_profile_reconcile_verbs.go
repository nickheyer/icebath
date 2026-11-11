package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runs fn as a task scheduled on the loop and waits up to timeout, timedOut when it is still running
func runOnLoopWithin(ctx context.Context, loop gateway.EventLoop, name string, timeout time.Duration,
	fn func(ctx context.Context) (any, error)) (result any, timedOut bool, err error) {
	fut := newGatewayFuture()
	if err := loop.CallSoonThreadsafe(func() {
		task := spawnGatewayTask(ctx, name, fn)
		task.addDoneCallback(func(f *gatewayFuture) { fut.settle(f.result, f.err) })
	}); err != nil {
		return nil, false, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err = fut.wait(waitCtx)
	if !fut.isDone() && ctx.Err() == nil {
		return nil, true, nil
	}
	return result, false, err
}

// Python type name and message of a failure, the f"{type(exc).__name__}: {exc}" text
func excText(err error) string {
	return pyrepr.ExceptionName(err) + ": " + pyrepr.ExceptionMessage(err)
}

// Ordered served profile homes as name and home pairs
func servedHomesOf(d interface {
	Keys() []string
	Get(string) (string, bool)
}) []profiles.ServedProfile {
	keys := d.Keys()
	out := make([]profiles.ServedProfile, 0, len(keys))
	for _, name := range keys {
		h, _ := d.Get(name)
		out = append(out, profiles.ServedProfile{Name: name, Home: h})
	}
	return out
}

// Serve or unserve control verb run on the owning loop, pending when the teardown outlasts five seconds
func profileLifecycleVerb(ctx context.Context, r *GatewayRunner, loop gateway.EventLoop, serve bool) gateway.ControlVerbHandler {
	refuse := func(msg string) (any, error) { return pyjson.ObjectOf("error", msg), nil }
	apply := func(ctx context.Context, nameValue any) (any, error) {
		if !r.multiplexOn() || !r.running.Load() || r.servedProfileHomes == nil {
			return refuse("host multiplexer is not ready")
		}
		lock := r.reconcileLock()
		lock.Lock()
		defer lock.Unlock()
		active := r.primaryProfileOrDefault()
		name, isStr := nameValue.(string)
		if !isStr || name == "" || name == active {
			return refuse("a non-launch profile name is required")
		}
		known := copyHomes(r.servedProfileHomes)
		if !serve {
			profileHome, ok := known.Get(name)
			if !ok {
				return refuse(fmt.Sprintf("profile '%s' is not served", name))
			}
			known.Delete(name)
			if err := r.unserveProfile(ctx, name, profileHome); err != nil {
				return nil, err
			}
			r.recordServedProfiles(ctx, active, servedHomesOf(known))
			return pyjson.ObjectOf("unserved", name, "served_profiles", namesList(r.servedProfileNames())), nil
		}
		listed, err := profiles.ProfilesToServe(ctx, true, false, true)
		if err != nil {
			return nil, err
		}
		installed := map[string]string{}
		for _, p := range listed {
			installed[p.Name] = p.Home
		}
		profileHome, ok := installed[name]
		if !ok {
			return refuse(fmt.Sprintf("unknown profile '%s'", name))
		}
		if name != "default" && profiles.ProfileIsParked(profileHome) {
			return refuse(fmt.Sprintf("profile '%s' is parked (gateway.parked)", name))
		}
		if known.Has(name) {
			return refuse(fmt.Sprintf("profile '%s' is already served", name))
		}
		known.Set(name, profileHome)
		result, err := r.applyProfileChanges(ctx, known, []string{name}, nil, nil, "control-socket", nil, nil)
		if err != nil {
			return nil, err
		}
		served, _ := result.Get("served_profiles")
		list, _ := served.([]any)
		if !slices.Contains(list, any(name)) {
			return refuse(fmt.Sprintf("profile '%s' was removed or parked during startup", name))
		}
		return pyjson.ObjectOf("served", name, "served_profiles", served), nil
	}
	return func(params map[string]any) (any, error) {
		result, timedOut, err := runOnLoopWithin(ctx, loop, "profile-lifecycle", 5*time.Second,
			func(c context.Context) (any, error) { return apply(c, params["name"]) })
		if timedOut {
			return pyjson.ObjectOf("pending", true, "served_profiles", namesList(r.servedProfileNames())), nil
		}
		if err != nil {
			reconcileLog().Warn("Profile lifecycle request failed", "exc_info", err)
			return pyjson.ObjectOf("error", excText(err)), nil
		}
		return result, nil
	}
}

// Control verb unserving one non launch profile of the multiplexer
func unserveProfileVerb(ctx context.Context, r *GatewayRunner, loop gateway.EventLoop) gateway.ControlVerbHandler {
	return profileLifecycleVerb(ctx, r, loop, false)
}

// Control verb serving one installed non launch profile in the multiplexer
func serveProfileVerb(ctx context.Context, r *GatewayRunner, loop gateway.EventLoop) gateway.ControlVerbHandler {
	return profileLifecycleVerb(ctx, r, loop, true)
}

// Trimmed str of a param, empty for a missing or falsy value
func paramText(params map[string]any, key string) string {
	v := params[key]
	if !pyrepr.Truthy(v) {
		return ""
	}
	return strings.TrimSpace(pyrepr.Str(v))
}

// Control verb a profile rename asks to rekey both durable stores and the live routing index
func migrateProfileIdentityVerb(ctx context.Context, r *GatewayRunner) gateway.ControlVerbHandler {
	return func(params map[string]any) (any, error) {
		old, fresh := paramText(params, "old"), paramText(params, "new")
		if old == "" || fresh == "" || old == fresh {
			return pyjson.ObjectOf("ok", false, "error", "old/new required and must differ"), nil
		}
		store := r.sessionStore
		if store == nil {
			return pyjson.ObjectOf("ok", false, "error", "live gateway has no session store"), nil
		}
		var acquired []*state.DB
		defer func() {
			for _, db := range acquired {
				state.ReleaseOrClose(db)
			}
		}()
		answer, err := func() (*pyjson.Object, error) {
			dbCounts := pyjson.NewObject()
			if rekeyer, ok := store.RoutingDB(ctx).(interface {
				RekeyProfileState(oldName, newName string) (map[string]int, error)
			}); ok && rekeyer != nil {
				counts, err := rekeyer.RekeyProfileState(old, fresh)
				if err != nil {
					return nil, err
				}
				dbCounts.Set("routing", counts)
			}
			if routingHome := store.RoutingHome(); routingHome != "" {
				profilePath := filepath.Join(routingHome, "profiles", fresh, "state.db")
				if _, statErr := os.Stat(profilePath); statErr == nil {
					profileDB, err := state.Acquire(ctx, profilePath)
					if err != nil {
						return nil, err
					}
					acquired = append(acquired, profileDB)
					counts, err := profileDB.RekeyProfileState(old, fresh)
					if err != nil {
						return nil, err
					}
					dbCounts.Set("profile", counts)
				}
			}
			rekeyed, err := store.RekeyProfileRouting(ctx, old, fresh)
			if err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("ok", true, "rekeyed", int64(rekeyed), "db", dbCounts), nil
		}()
		if err != nil {
			reconcileLog().Warn(fmt.Sprintf("Profile identity migration failed for %s->%s: %s", pyrepr.Repr(old), pyrepr.Repr(fresh), err))
			return pyjson.ObjectOf("ok", false, "error", excText(err)), nil
		}
		return answer, nil
	}
}

// Control verb a profile delete asks to drop the durable rows and live routing entries of the profile
func purgeProfileIdentityVerb(ctx context.Context, r *GatewayRunner) gateway.ControlVerbHandler {
	return func(params map[string]any) (any, error) {
		name := paramText(params, "name")
		if name == "" {
			return pyjson.ObjectOf("ok", false, "error", "name required"), nil
		}
		store := r.sessionStore
		if store == nil {
			return pyjson.ObjectOf("ok", false, "error", "live gateway has no session store"), nil
		}
		answer, err := func() (*pyjson.Object, error) {
			dbCounts := pyjson.NewObject()
			if purger, ok := store.RoutingDB(ctx).(interface {
				PurgeProfileState(profile string) (map[string]int, error)
			}); ok && purger != nil {
				counts, err := purger.PurgeProfileState(name)
				if err != nil {
					return nil, err
				}
				dbCounts.Set("routing", counts)
			}
			dropped, err := store.PurgeProfileRouting(ctx, name)
			if err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("ok", true, "dropped", int64(dropped), "db", dbCounts), nil
		}()
		if err != nil {
			reconcileLog().Warn(fmt.Sprintf("Profile identity purge failed for %s: %s", pyrepr.Repr(name), err))
			return pyjson.ObjectOf("ok", false, "error", excText(err)), nil
		}
		return answer, nil
	}
}
