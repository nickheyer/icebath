package manage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Control verb and store opener the identity rekey goes through, swapped by tests
var (
	migrateGatewayIdentity = gatewayMigrateProfileIdentity
	acquireStateDB         = state.Acquire
)

// Retries the durable identity migration of a completed rename, true when every applicable identity moved
func MigrateProfileIdentity(ctx context.Context, oldName, newName string) (bool, error) {
	oldCanon, err := profiles.CanonValid(oldName)
	if err != nil {
		return false, err
	}
	newCanon, err := profiles.CanonValid(newName)
	if err != nil {
		return false, err
	}
	if oldCanon == "default" || newCanon == "default" {
		return false, &profiles.ValueError{Msg: "Identity migration applies to named profiles only."}
	}
	dir, err := profiles.GetProfileDir(newCanon)
	if err != nil {
		return false, err
	}
	if !isDir(dir) {
		return false, profiles.UnknownProfileError(newCanon)
	}
	return migrateProfileIdentity(ctx, oldCanon, newCanon, liveDefaultMultiplexer()), nil
}

// Purges a deleted profile's routing identity, refusing a name that is a live profile again
func PurgeProfileIdentity(ctx context.Context, profile string) (bool, error) {
	canon, err := profiles.CanonValid(profile)
	if err != nil {
		return false, err
	}
	if canon == "default" {
		return false, &profiles.ValueError{Msg: "Identity purge applies to named profiles only."}
	}
	if profiles.ProfileExists(canon) {
		return false, &profiles.ValueError{Msg: fmt.Sprintf("Profile '%s' exists; purge-identity only settles the identity of a delete that "+
			"has already completed.", canon)}
	}
	return purgeProfileIdentity(ctx, canon, liveDefaultMultiplexer()), nil
}

// Python style type name and message of an error for warnings
func excText(err error) string {
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return fmt.Sprintf("%s: %v", t.Name(), err)
}

// Reports whether a decoded control answer is a mapping with ok exactly true
func answerOK(answer any) bool {
	m, isMap := answer.(*pyjson.Object)
	if !isMap {
		return false
	}
	v, _ := m.Get("ok")
	ok, isBool := v.(bool)
	return isBool && ok
}

// Purges identity through the live gateway verb or the durable stores, false when the settlement is pending
func purgeProfileIdentity(ctx context.Context, canon string, liveMux bool) bool {
	root := profiles.DefaultHome()
	if liveMux {
		var reason string
		answer, err := gatewayPurgeProfileIdentity(root, canon)
		if err != nil {
			reason = excText(err)
		} else {
			if answerOK(answer) {
				return true
			}
			reason = controlAnswerFailure(answer)
			if answer == nil && gatewayAcceptsProfileIdentityVerb(root) {
				reason += " — the gateway is running but does not implement 'purge-profile-identity' (an older process than this CLI)"
			}
		}
		fmt.Fprintf(stderr, "⚠ Profile was deleted, but the live gateway could not purge its session identity (%s). Restart the gateway, then run:\n"+
			"    icebath profile purge-identity %s\n", reason, canon)
		return false
	}
	dir, err := profiles.GetProfileDir(canon)
	if err != nil {
		return false
	}
	purged := true
	for _, dbPath := range []string{filepath.Join(root, "state.db"), filepath.Join(dir, "state.db")} {
		if !pathExists(dbPath) {
			continue
		}
		db, err := acquireStateDB(ctx, dbPath)
		if err == nil {
			_, err = db.PurgeProfileState(canon)
			state.ReleaseOrClose(db)
		}
		if err != nil {
			purged = false
			fmt.Fprintf(stderr, "⚠ Profile was deleted, but identity purge failed for %s: %s\n", dbPath, excText(err))
		}
	}
	return purged
}

// Why a control socket answer is not a success, keeping the raw answer when it names no reason
func controlAnswerFailure(answer any) string {
	if m, ok := answer.(*pyjson.Object); ok {
		for _, key := range []string{"error", "message", "detail"} {
			if v, _ := m.Get(key); truthy(v) {
				return pyrepr.Str(v)
			}
		}
		return pyrepr.Repr(m)
	}
	if answer != nil {
		return pyrepr.Repr(answer)
	}
	return "no response from gateway control socket"
}

// Reports whether a gateway answers its always present identify verb at root
func gatewayAcceptsProfileIdentityVerb(root string) bool {
	answer, err := gatewayIdentify(root)
	return err == nil && answer != nil
}

// Rekeys checkpoint projects whose absolute workdirs moved with the profile directory
func migrateCheckpointIdentity(oldCanon, newCanon string) bool {
	oldDir, err1 := profiles.GetProfileDir(oldCanon)
	newDir, err2 := profiles.GetProfileDir(newCanon)
	if err1 != nil || err2 != nil {
		return false
	}
	result := tools.MigrateProfileCheckpointProjects(nil, oldDir, newDir)
	if result.Migrated > 0 {
		fmt.Fprintf(stdout, "✓ Checkpoint identity updated: %d project(s)\n", result.Migrated)
	}
	if result.Errors == 0 {
		return true
	}
	fmt.Fprintf(stderr, "⚠ Profile was renamed, but checkpoint identity migration failed for %d project(s). Retry with:\n"+
		"    icebath profile migrate-identity %s %s\n", result.Errors, oldCanon, newCanon)
	return false
}

// Rekeys renamed profile identity through the live gateway or the durable stores, true when all of it moved
func migrateProfileIdentity(ctx context.Context, oldCanon, newCanon string, liveMux bool) bool {
	checkpointMigrated := migrateCheckpointIdentity(oldCanon, newCanon)
	root := profiles.DefaultHome()
	if liveMux {
		var reason string
		answer, err := migrateGatewayIdentity(root, oldCanon, newCanon)
		if err != nil {
			reason = excText(err)
		} else {
			if answerOK(answer) {
				return checkpointMigrated
			}
			reason = controlAnswerFailure(answer)
			if answer == nil && gatewayAcceptsProfileIdentityVerb(root) {
				reason += " — the gateway is running but does not implement 'migrate-profile-identity' (an older process than this CLI)"
			}
		}
		fmt.Fprintf(stderr, "⚠ Profile was renamed, but the live gateway could not migrate session identity (%s). Restart the gateway, then run:\n"+
			"    icebath profile migrate-identity %s %s\n", reason, oldCanon, newCanon)
		return false
	}
	newDir, err := profiles.GetProfileDir(newCanon)
	if err != nil {
		return false
	}
	migrated := checkpointMigrated
	for _, dbPath := range []string{filepath.Join(root, "state.db"), filepath.Join(newDir, "state.db")} {
		if !pathExists(dbPath) {
			continue
		}
		db, err := acquireStateDB(ctx, dbPath)
		if err == nil {
			_, err = db.RekeyProfileState(oldCanon, newCanon)
			state.ReleaseOrClose(db)
		}
		if err != nil {
			migrated = false
			fmt.Fprintf(stderr, "⚠ Profile was renamed, but identity migration failed for %s: %s\n", dbPath, excText(err))
		}
	}
	return migrated
}

// Python truthiness of a decoded JSON or YAML value
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case *pyjson.Object:
		return x.Len() > 0
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map, reflect.String:
		return rv.Len() > 0
	}
	return true
}

// Reports whether a path is a directory following symlinks
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
