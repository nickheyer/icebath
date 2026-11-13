package vault

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Display name of the Bitwarden backend
const bitwardenDisplay = "Bitwarden"

// Variable the master password reaches bw unlock through, set only in the child environment
const bitwardenPasswordEnv = "ICEBATH_BW_MASTER"

// Variables of the launch environment the Bitwarden CLI may see
var bitwardenEnvKeep = []string{"PATH", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot", "TMPDIR", "TMP", "TEMP",
	"XDG_CONFIG_HOME", "BITWARDENCLI_APPDATA_DIR"}

// Bitwarden Password Manager logins read through the bw CLI
type bitwarden struct {
	vault *Vault
	opts  BitwardenOptions
}

// Config key of the Bitwarden backend
func (*bitwarden) Name() string { return bitwardenName }

// Name shown for Bitwarden
func (*bitwarden) DisplayName() string { return bitwardenDisplay }

// Handle prefix of Bitwarden items
func (*bitwarden) Prefix() string { return bitwardenPrefix }

// Reports true, Bitwarden asks for a master password per session
func (*bitwarden) NeedsUnlock() bool { return true }

// Reports whether handle names a Bitwarden item
func (*bitwarden) Owns(handle string) bool { return strings.HasPrefix(handle, bitwardenPrefix) }

// Reports whether this session holds an unlock, without refreshing its idle time
func (b *bitwarden) Unlocked(ctx context.Context) bool {
	return b.vault.hasSession(ctx, bitwardenName)
}

// Path of the Bitwarden CLI, or the remedy when it is missing
func (b *bitwarden) bw() (string, error) {
	if path := findBw(b.opts.BinaryPath); path != "" {
		return path, nil
	}
	return "", errors.New("Bitwarden CLI (bw) not found — install it or set vault.bitwarden.binary_path")
}

// Allowlisted child environment carrying the session token when there is one
func (b *bitwarden) env(sessionToken string) map[string]string {
	env := environment(bitwardenEnvKeep...)
	env["NO_COLOR"] = "1"
	if sessionToken != "" {
		env["BW_SESSION"] = sessionToken
	}
	return env
}

// Mints a session key with bw unlock, the master password reaching bw only through the child's environment
func (b *bitwarden) Unlock(ctx context.Context, masterPassword string) error {
	key, err := b.vault.sessionKey(ctx, bitwardenName)
	if err != nil {
		return err
	}
	generation := b.vault.sessions.begin(key)
	bw, err := b.bw()
	if err != nil {
		return err
	}
	env := b.env("")
	env[bitwardenPasswordEnv] = masterPassword
	res, err := runManager(ctx, managerCall{path: bw, args: []string{"unlock", "--raw", "--nointeraction", "--passwordenv", bitwardenPasswordEnv},
		env: env, label: "bw", timeoutMessage: fmt.Sprintf("bw unlock timed out after %.0fs", managerTimeout.Seconds())})
	if err != nil {
		return err
	}
	token := trimSpace(res.stdout)
	if !res.ok || token == "" {
		detail := head(trimSpace(scrubBw(res.stderr)), 200)
		if strings.Contains(lowerFull(detail), "not logged in") {
			detail = "not logged in — run `bw login` once in a terminal first"
		}
		if detail == "" {
			detail = "no session key"
		}
		return fmt.Errorf("Bitwarden unlock failed: %s", detail)
	}
	if !b.vault.sessions.commit(key, token, generation, sessionID(ctx)) {
		return errors.New("Bitwarden was locked while unlocking; try again")
	}
	return nil
}

// Output of one bw call under this session's token, a locked vault or lost session locking the backend
func (b *bitwarden) run(ctx context.Context, args ...string) (string, error) {
	token, err := b.vault.managerToken(ctx, bitwardenName, true)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", &UnlockRequiredError{Backend: bitwardenName, DisplayName: bitwardenDisplay}
	}
	bw, err := b.bw()
	if err != nil {
		return "", err
	}
	res, err := runManager(ctx, managerCall{path: bw, args: append(args, "--nointeraction"), env: b.env(token), label: "bw",
		timeoutMessage: "bw timed out"})
	if err != nil {
		return "", err
	}
	if !res.ok {
		detail := scrubBw(res.stderr)
		lower := lowerFull(detail)
		if strings.Contains(lower, "locked") || strings.Contains(lower, "session") {
			if err := b.vault.Lock(ctx, bitwardenName); err != nil {
				return "", err
			}
			return "", &UnlockRequiredError{Backend: bitwardenName, DisplayName: bitwardenDisplay}
		}
		return "", fmt.Errorf("bw failed: %s", head(detail, 200))
	}
	return res.stdout, nil
}

// Login items with at least one fillable URI, nothing while locked
func (b *bitwarden) List(ctx context.Context) ([]ItemMeta, error) {
	if !b.Unlocked(ctx) {
		return nil, nil
	}
	out, err := b.run(ctx, "list", "items")
	if err != nil {
		return nil, err
	}
	items, err := listedItems(out, "bw")
	if err != nil {
		return nil, err
	}
	var metas []ItemMeta
	for _, item := range items {
		login, isLogin := objectMembers(lookup(item, "login"))
		if !numberIs(lookup(item, "type"), 1) || !isLogin {
			continue
		}
		uris, err := loginURIs(lookup(login, "uris"))
		if err != nil {
			return nil, err
		}
		var origins []string
		for _, uri := range uris {
			if numberIs(lookup(uri, "match"), 5) {
				continue
			}
			origin, err := NormalizeOrigin(truthyText(lookup(uri, "uri")))
			if err != nil || origin == "" || slices.Contains(origins, origin) {
				continue
			}
			origins = append(origins, origin)
		}
		if len(origins) == 0 {
			continue
		}
		meta := ItemMeta{ID: bitwardenPrefix + valueText(lookup(item, "id")), Kind: "login", Label: truthyText(lookup(item, "name")),
			Origin: origins[0], CreatedAt: truthyText(lookup(item, "creationDate")), AllowedOrigins: webOrigins(origins)}
		if meta.Label == "" {
			meta.Label = origins[0]
		}
		if user := trimSpace(truthyText(lookup(login, "username"))); user != "" {
			meta.Identifier, meta.IdentifierType = user, "username"
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

// Entries of each URI of a login, none when missing, an error for anything but a list of objects
func loginURIs(v jsontext.Value) ([][]member, error) {
	if !truthy(v) {
		return nil, nil
	}
	entries, ok := arrayItems(v)
	if !ok {
		return nil, errors.New("bw returned a login whose uris are not a list")
	}
	out := make([][]member, 0, len(entries))
	for _, entry := range entries {
		uri, ok := objectMembers(entry)
		if !ok {
			return nil, errors.New("bw returned a login uri that is not an object")
		}
		out = append(out, uri)
	}
	return out, nil
}

// Listed item carrying handle
func (b *bitwarden) Meta(ctx context.Context, handle string) (ItemMeta, bool, error) {
	metas, err := b.List(ctx)
	if err != nil {
		return ItemMeta{}, false, err
	}
	for _, m := range metas {
		if m.ID == handle {
			return m, true, nil
		}
	}
	return ItemMeta{}, false, nil
}

// Password of the login, read for the fill only
func (b *bitwarden) ResolvePassword(ctx context.Context, handle string) (Secret, error) {
	out, err := b.run(ctx, "get", "password", handleID(handle, bitwardenPrefix))
	if err != nil {
		return Secret{}, err
	}
	return secretOf(secretField{PasswordField, strings.TrimRight(out, "\r\n")}), nil
}

// Current code from the login's TOTP seed, none when it lacks one or bw fails so the user is asked
func (b *bitwarden) ResolveOTP(ctx context.Context, handle string) (Secret, error) {
	out, err := b.run(ctx, "get", "totp", handleID(handle, bitwardenPrefix))
	if err != nil {
		return Secret{}, ctx.Err()
	}
	return codeSecret(trimSpace(out)), nil
}

// Password only payload, the only shape a manager login has
func (b *bitwarden) ResolveSecret(ctx context.Context, handle string) (Secret, error) {
	return b.ResolvePassword(ctx, handle)
}

// Runs fill directly, Bitwarden freezing nothing for it
func (*bitwarden) Fill(ctx context.Context, fill func(context.Context) error) error { return fill(ctx) }

// Reports false, a Bitwarden fill needs no re-read
func (*bitwarden) Rereads() bool { return false }
