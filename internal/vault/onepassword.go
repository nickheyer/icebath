package vault

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Display name of the 1Password backend
const onePasswordDisplay = "1Password"

// Variables of the launch environment the 1Password CLI may see
var onePasswordEnvKeep = []string{"PATH", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot", "TMPDIR", "TMP",
	"TEMP", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "OP_CONFIG_DIR", "OP_ACCOUNT", "OP_LOAD_DESKTOP_APP_SETTINGS"}

// 1Password Login items with a website, read through the op CLI
type onePassword struct {
	vault        *Vault
	opts         OnePasswordOptions
	serviceToken func() string
	mu           sync.Mutex
	itemVaults   map[string]string
}

// 1Password backend whose service account token, when configured, skips the unlock prompt
func (v *Vault) newOnePassword(ctx context.Context, opts OnePasswordOptions) *onePassword {
	tokenEnv := opts.ServiceAccountTokenEnv
	if tokenEnv == "" {
		tokenEnv = "OP_SERVICE_ACCOUNT_TOKEN"
	}
	return &onePassword{vault: v, opts: opts, serviceToken: seal(v.secrets(ctx, tokenEnv)), itemVaults: map[string]string{}}
}

// Service account token of the profile, empty when it has none
func (b *onePassword) service() string { return unseal(b.serviceToken) }

// Vault id the latest listing reported for an item and whether it listed the item
func (b *onePassword) itemVault(id string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	vault, ok := b.itemVaults[id]
	return vault, ok
}

// Config key of the 1Password backend
func (*onePassword) Name() string { return onePasswordName }

// Name shown for 1Password
func (*onePassword) DisplayName() string { return onePasswordDisplay }

// Handle prefix of 1Password items
func (*onePassword) Prefix() string { return onePasswordPfx }

// Reports true, 1Password asks for a master password per session
func (*onePassword) NeedsUnlock() bool { return true }

// Reports whether handle names a 1Password item
func (*onePassword) Owns(handle string) bool { return strings.HasPrefix(handle, onePasswordPfx) }

// Reports whether a service account token is configured or this session holds an unlock
func (b *onePassword) Unlocked(ctx context.Context) bool {
	return b.service() != "" || b.vault.hasSession(ctx, onePasswordName)
}

// Path of the 1Password CLI, or the remedy when it is missing
func (b *onePassword) op() (string, error) {
	if path := findOp(b.opts.BinaryPath); path != "" {
		return path, nil
	}
	return "", errors.New("1Password CLI (op) not found — install it or set vault.onepassword.binary_path")
}

// Allowlisted child environment with the profile's Connect credentials, the account and the auth to use
func (b *onePassword) env(ctx context.Context, sessionToken string) map[string]string {
	env := environment(onePasswordEnvKeep...)
	for _, name := range []string{"OP_CONNECT_HOST", "OP_CONNECT_TOKEN"} {
		if v := b.vault.secrets(ctx, name); v != "" {
			env[name] = v
		}
	}
	env["NO_COLOR"] = "1"
	if b.opts.Account != "" {
		env["OP_ACCOUNT"] = b.opts.Account
	}
	switch {
	case b.service() != "":
		env["OP_SERVICE_ACCOUNT_TOKEN"] = b.service()
	case sessionToken != "" && b.opts.Account != "":
		env["OP_SESSION_"+b.opts.Account] = sessionToken
	case sessionToken != "":
		env["OP_SESSION"] = sessionToken
	}
	return env
}

// Mints a session token with op signin from the master password, which goes to op on stdin only
func (b *onePassword) Unlock(ctx context.Context, masterPassword string) error {
	key, err := b.vault.sessionKey(ctx, onePasswordName)
	if err != nil {
		return err
	}
	generation := b.vault.sessions.begin(key)
	op, err := b.op()
	if err != nil {
		return err
	}
	args := []string{"signin", "--raw"}
	if b.opts.Account != "" {
		args = append(args, "--account", b.opts.Account)
	}
	res, err := runManager(ctx, managerCall{path: op, args: args, env: b.env(ctx, ""), input: masterPassword + "\n", hasInput: true,
		label: "op", timeoutMessage: fmt.Sprintf("op unlock timed out after %.0fs", managerTimeout.Seconds())})
	if err != nil {
		return err
	}
	token := trimSpace(res.stdout)
	if !res.ok || token == "" {
		detail := head(scrubOp(withoutSecrets(res.stderr, masterPassword)), 200)
		if detail == "" {
			detail = "no session token"
		}
		return fmt.Errorf("1Password unlock failed: %s", detail)
	}
	if !b.vault.sessions.commit(key, token, generation, sessionID(ctx)) {
		return errors.New("1Password was locked while unlocking; try again")
	}
	return nil
}

// Output of one op call under the service account or this session's token, a lost session locking the backend
func (b *onePassword) run(ctx context.Context, args ...string) (string, error) {
	token := ""
	if b.service() == "" {
		var err error
		if token, err = b.vault.managerToken(ctx, onePasswordName, true); err != nil {
			return "", err
		}
		if token == "" {
			return "", &UnlockRequiredError{Backend: onePasswordName, DisplayName: onePasswordDisplay}
		}
	}
	op, err := b.op()
	if err != nil {
		return "", err
	}
	res, err := runManager(ctx, managerCall{path: op, args: args, env: b.env(ctx, token), label: "op", timeoutMessage: "op timed out"})
	if err != nil {
		return "", err
	}
	if !res.ok {
		detail := scrubOp(withoutSecrets(res.stderr, token, b.service()))
		lower := lowerFull(detail)
		if b.service() == "" && (strings.Contains(lower, "session") || strings.Contains(lower, "sign in") || strings.Contains(lower, "not signed in")) {
			if err := b.vault.Lock(ctx, onePasswordName); err != nil {
				return "", err
			}
			return "", &UnlockRequiredError{Backend: onePasswordName, DisplayName: onePasswordDisplay}
		}
		return "", fmt.Errorf("op failed: %s", head(detail, 200))
	}
	return res.stdout, nil
}

// Login items with at least one parseable website, nothing while locked
func (b *onePassword) List(ctx context.Context) ([]ItemMeta, error) {
	if !b.Unlocked(ctx) {
		return nil, nil
	}
	out, err := b.run(ctx, "item", "list", "--categories", "Login", "--format", "json")
	if err != nil {
		return nil, err
	}
	items, err := listedItems(out, "op")
	if err != nil {
		return nil, err
	}
	vaults := map[string]string{}
	b.mu.Lock()
	b.itemVaults = vaults
	b.mu.Unlock()
	var metas []ItemMeta
	for _, item := range items {
		id := lookup(item, "id")
		if truthy(id) {
			vaultID := ""
			if vault, ok := objectMembers(lookup(item, "vault")); ok {
				vaultID = truthyText(lookup(vault, "id"))
			}
			b.mu.Lock()
			vaults[valueText(id)] = vaultID
			b.mu.Unlock()
		}
		urls, err := itemURLs(lookup(item, "urls"))
		if err != nil {
			return nil, err
		}
		origins := allOrigins(urls)
		if len(origins) == 0 {
			continue
		}
		meta := ItemMeta{ID: onePasswordPfx + valueText(id), Kind: "login", Label: truthyText(lookup(item, "title")), Origin: origins[0],
			CreatedAt: truthyText(lookup(item, "created_at")), AllowedOrigins: webOrigins(origins)}
		if meta.Label == "" {
			meta.Label = origins[0]
		}
		if username := trimSpace(truthyText(lookup(item, "additional_information"))); username != "" {
			meta.Identifier, meta.IdentifierType = username, "username"
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

// Website hrefs of a listed item, a missing or empty list holding none and a scalar list an error
func itemURLs(v jsontext.Value) ([]string, error) {
	if !truthy(v) {
		return nil, nil
	}
	entries, ok := arrayItems(v)
	switch {
	case !ok && (v.Kind() == jsontext.KindNumber || v.Kind() == jsontext.KindTrue):
		return nil, errors.New("op returned an item whose urls are not a list")
	case !ok:
		return nil, nil
	}
	var urls []string
	for _, entry := range entries {
		if u, ok := objectMembers(entry); ok {
			if href := lookup(u, "href"); truthy(href) {
				urls = append(urls, valueText(href))
			}
		}
	}
	return urls, nil
}

// Listed item carrying handle
func (b *onePassword) Meta(ctx context.Context, handle string) (ItemMeta, bool, error) {
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

// op item get target of a listed login with the vault the listing reported, refused for anything unlisted
func (b *onePassword) selector(ctx context.Context, handle string) ([]string, error) {
	id := handleID(handle, onePasswordPfx)
	if _, ok := b.itemVault(id); !ok {
		if _, err := b.List(ctx); err != nil {
			return nil, err
		}
	}
	vaultID, ok := b.itemVault(id)
	if !ok {
		return nil, errors.New("1Password login not found; list logins again")
	}
	if vaultID != "" {
		return []string{id, "--vault", vaultID}, nil
	}
	return []string{id}, nil
}

// Password field of the login, revealed for the fill only
func (b *onePassword) ResolvePassword(ctx context.Context, handle string) (Secret, error) {
	target, err := b.selector(ctx, handle)
	if err != nil {
		return Secret{}, err
	}
	out, err := b.run(ctx, append(append([]string{"item", "get"}, target...), "--fields", "label=password", "--reveal")...)
	if err != nil {
		return Secret{}, err
	}
	return secretOf(secretField{PasswordField, strings.TrimRight(out, "\r\n")}), nil
}

// Current code of the item's one time password field, none when op has none so the user is asked
func (b *onePassword) ResolveOTP(ctx context.Context, handle string) (Secret, error) {
	target, err := b.selector(ctx, handle)
	if err != nil {
		return Secret{}, ctx.Err()
	}
	out, err := b.run(ctx, append(append([]string{"item", "get"}, target...), "--otp")...)
	if err != nil {
		return Secret{}, ctx.Err()
	}
	return codeSecret(trimSpace(out)), nil
}

// Password only payload, the only shape a manager login has
func (b *onePassword) ResolveSecret(ctx context.Context, handle string) (Secret, error) {
	return b.ResolvePassword(ctx, handle)
}

// Runs fill directly, 1Password freezing nothing for it
func (*onePassword) Fill(ctx context.Context, fill func(context.Context) error) error {
	return fill(ctx)
}

// Reports false, a 1Password fill needs no re-read
func (*onePassword) Rereads() bool { return false }
