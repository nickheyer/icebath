package portal

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/auth"
)

const sharedRenewalKey = "renewal"

var sharedStateKeys = []string{
	"access_token", "refresh_token", "token_type", "scope", "client_id", "portal_base_url",
	"inference_base_url", "obtained_at", "expires_at",
	"auth_method", "account_tier", "anon_token", "user_id", "org_id",
}

func (a *Account) sharedDir() string {
	if override := strings.TrimSpace(os.Getenv(EnvSharedAuthDir)); override != "" {
		return home.ExpandUser(override)
	}
	return filepath.Join(a.root, string(home.Shared))
}

func (a *Account) sharedStorePath() string {
	return filepath.Join(a.sharedDir(), filepath.Base(string(home.PortalSharedAuth)))
}

func lockPathFor(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + ".lock"
}

func openFileLock(ctx context.Context, lockPath string, timeout time.Duration, message string) (context.Context, func(), error) {
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	lk, err := fileutil.OpenLock(wait, lockPath, fileutil.RecordHolder(), fileutil.Range(0, 1), fileutil.TimeoutMessage(message))
	if err != nil {
		return ctx, nil, err
	}
	return lk.Context(ctx), func() { _ = lk.Release() }, nil
}

func (a *Account) lockShared(ctx context.Context, timeout time.Duration) (context.Context, func(), error) {
	lockPath := lockPathFor(a.sharedStorePath())
	return openFileLock(ctx, lockPath, timeout, fmt.Sprintf("Timed out waiting for shared Icebath Portal auth lock (%s)", lockPath))
}

func sharedLockTimeout(timeout time.Duration) time.Duration {
	return max(timeout+5*time.Second, auth.LockTimeout)
}

func (a *Account) savePrivateJSON(ctx context.Context, target string, data map[string]any) error {
	if err := a.homes.Mkdir(ctx, filepath.Dir(target)); err != nil {
		return err
	}
	if err := home.SecureParentDir(target); err != nil {
		return err
	}
	base := filepath.Base(target)
	return fileutil.WriteJSON(ctx, target, data, fileutil.JSONStyle{Indent: 2, Sorted: true},
		fileutil.Private(), fileutil.TempPrefix("."+strings.TrimSuffix(base, filepath.Ext(base))+"_"))
}

type identity struct {
	kind  string
	value any
}

func identityOf(state *object) identity {
	if isGuestState(state) {
		return identity{kind: "guest", value: state.get("anon_token")}
	}
	return identity{kind: "account", value: claimsOf(state.get("access_token")).get("sub")}
}

func (i identity) equal(other identity) bool {
	return i.kind == other.kind && sameValue(i.value, other.value)
}

func sameKnownIdentity(x, y *object) bool {
	id := identityOf(x)
	return present(id.value) && id.equal(identityOf(y))
}

func stateOrder(x, y *object) (int, bool) {
	return auth.OAuthPairOrder(x.raw(), y.raw())
}

func sharedShape(src *object) *object {
	orDefault := func(key, def string) any {
		if v := src.get(key); truthy(v) {
			return v
		}
		return def
	}
	inferenceDefault := auth.DefaultPortalInferenceURL
	if src.str("auth_method") == AnonAuthMethod {
		inferenceDefault = auth.DefaultPortalWelcomeURL
	}
	out := pairs(
		"access_token", src.get("access_token"), "refresh_token", src.get("refresh_token"),
		"token_type", orDefault("token_type", "Bearer"),
		"scope", orDefault("scope", auth.DefaultPortalScope),
		"client_id", orDefault("client_id", auth.DefaultPortalClientID),
		"portal_base_url", orDefault("portal_base_url", auth.DefaultPortalURL),
		"inference_base_url", orDefault("inference_base_url", inferenceDefault),
		"obtained_at", src.get("obtained_at"), "expires_at", src.get("expires_at"),
	)
	for _, k := range []string{"auth_method", "account_tier", "anon_token", "user_id", "org_id", "connectors_token", auth.WelcomeClaimFlag} {
		if v := src.get(k); present(v) {
			out.set(k, cloneValue(v))
		}
	}
	return out
}

func (a *Account) readShared() *object {
	path := a.sharedStorePath()
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Debug(fmt.Sprintf("Shared Icebath Portal auth store at %s is unreadable", path), "error", err)
		return nil
	}
	v, err := parseValue(data)
	if err != nil {
		slog.Debug(fmt.Sprintf("Shared Icebath Portal auth store at %s is unreadable", path), "error", err)
		return nil
	}
	payload, ok := v.(*object)
	if !ok || payload == nil {
		return nil
	}
	if nonEmpty(payload.get("anon_token")) || (nonEmpty(payload.get("access_token")) && nonEmpty(payload.get("refresh_token"))) {
		return payload
	}
	return nil
}

func sharedCopyIsNewer(held, state *object, same bool) bool {
	if held == nil || isGuestState(state) {
		return false
	}
	candidate := held
	if !same {
		candidate = held.obj(sharedRenewalKey)
	}
	if candidate == nil || !sameKnownIdentity(candidate, state) {
		return false
	}
	order, known := stateOrder(candidate, state)
	return known && order == 1
}

func (a *Account) writeShared(ctx context.Context, state *object, renewal bool) {
	refreshToken := state.get("refresh_token")
	if !nonEmpty(state.get("anon_token")) && !(nonEmpty(refreshToken) && nonEmpty(state.get("access_token"))) {
		return
	}
	shared := pairs("_schema", 1)
	shared.merge(sharedShape(state))
	shared.set("updated_at", isoUTC(time.Now()))
	path := a.sharedStorePath()
	err := func() error {
		lctx, unlock, err := a.lockShared(ctx, auth.LockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		held := a.readShared()
		same := held != nil && identityOf(held).equal(identityOf(state))
		if renewal && sharedCopyIsNewer(held, state, same) {
			oauthTrace("portal_shared_store_write_skipped_older", "", map[string]any{"path": path, "refresh_token_fp": tokenFingerprint(refreshToken)})
			return errSharedWriteSkipped
		}
		if renewal && held != nil && !same {
			shared = held.clone()
			shared.set(sharedRenewalKey, sharedShape(state))
		} else {
			if err := a.noteReplacement(lctx, held, state); err != nil {
				return err
			}
			if same && held.has(sharedRenewalKey) {
				shared.set(sharedRenewalKey, cloneValue(held.get(sharedRenewalKey)))
			}
		}
		return a.savePrivateJSON(lctx, path, shared.plain())
	}()
	if errors.Is(err, errSharedWriteSkipped) {
		return
	}
	if err != nil {
		slog.Debug("Failed to write shared Icebath Portal auth store", "error", err)
		return
	}
	oauthTrace("portal_shared_store_written", "", map[string]any{"path": path, "refresh_token_fp": tokenFingerprint(refreshToken)})
}

var errSharedWriteSkipped = errors.New("portal: shared store already holds a newer copy")

func (a *Account) clearShared(ctx context.Context, reason string) {
	_, unlock, err := a.lockShared(ctx, auth.LockTimeout)
	if err == nil {
		err = os.Remove(a.sharedStorePath())
		unlock()
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	if err != nil {
		slog.Debug("Failed to clear shared Icebath Portal auth store", "error", err)
		return
	}
	oauthTrace("portal_shared_store_cleared", "", map[string]any{"reason": reason})
}

func (a *Account) mergeShared(ctx context.Context, state *object) bool {
	shared := a.readShared()
	if shared == nil {
		shared = newObject()
	}
	if renewed := shared.obj(sharedRenewalKey); renewed != nil && identityOf(renewed).equal(identityOf(state)) {
		shared = renewed
	}
	sharedRefresh, _ := shared.get("refresh_token").(string)
	if !nonEmpty(sharedRefresh) {
		return false
	}
	refreshChanged := strings.TrimSpace(sharedRefresh) != strings.TrimSpace(textualOrEmpty(state.get("refresh_token")))
	order, known := stateOrder(shared, state)
	knownSame := sameKnownIdentity(shared, state)
	same := knownSame || !nonEmpty(state.get("access_token"))
	if !(same && ((known && order == 1) || (!known && refreshChanged))) {
		if known && order == -1 && refreshChanged && knownSame {
			oauthTrace("portal_shared_store_older_than_profile", "", map[string]any{
				"shared_refresh_token_fp": tokenFingerprint(sharedRefresh), "refresh_token_fp": tokenFingerprint(state.get("refresh_token"))})
			a.writeShared(ctx, state, true)
		}
		return false
	}
	for _, k := range sharedStateKeys {
		if v := shared.get(k); present(v) {
			state.set(k, cloneValue(v))
		}
	}
	return true
}

func textualOrEmpty(v any) string {
	if !truthy(v) {
		return ""
	}
	return textual(v)
}
