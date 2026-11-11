package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/vault"
)

const vaultManagerTimeout = 30 * time.Second

// Profile scoped secret reader of the manager backends, the module attribute hermes tests patch
var vaultGetSecret = provider.GetSecretStr

// A manager backend the unlock action can hand the master password to
type unlockableLoginBackend interface {
	Unlock(masterPassword string) error
}

// Entries of a parsed JSON value that must each be an object, any other entry raising the attribute error
func managerItems(raw any) ([]*pyjson.Object, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, nil
	}
	out := make([]*pyjson.Object, 0, len(list))
	for _, entry := range list {
		obj, ok := pyjson.AsObject(entry)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(entry))
		}
		out = append(out, obj)
	}
	return out, nil
}

// Members of a truthy list value, nothing for a falsy one
func managerList(v any) []any {
	if !pyrepr.Truthy(v) {
		return nil
	}
	list, _ := v.([]any)
	return list
}

// Browser page origins of an item, the first origin alone when none of them is a web origin
func webOrigins(origins []string) []string {
	web := []string{}
	for _, o := range origins {
		if strings.HasPrefix(o, "http://") || strings.HasPrefix(o, "https://") {
			web = append(web, o)
		}
	}
	if len(web) > 0 {
		return web
	}
	return []string{origins[0]}
}

// Every normalized origin saved on an item, deduped with order preserved
func allOrigins(urls []string) []string {
	out := []string{}
	for _, u := range urls {
		origin, err := vault.NormalizeOrigin(u)
		if err != nil {
			continue
		}
		if !hasToken(out, origin) {
			out = append(out, origin)
		}
	}
	return out
}

func managerIdentifier(v any) (any, any) {
	username := pystr.Strip(vaultText(v))
	if username == "" {
		return nil, nil
	}
	return "username", username
}

func findMeta(items []vault.VaultItemMeta, handle string) *vault.VaultItemMeta {
	for i := range items {
		if items[i].ID == handle {
			return &items[i]
		}
	}
	return nil
}

// 1Password Login items read through the op CLI
type OnePasswordLoginBackend struct {
	LoginBackendDefaults
	ctx          context.Context
	cfg          map[string]any
	serviceToken string
	itemVaults   map[string]string
	run          func(args ...string) (string, error)
	unlocked     func() bool
}

// 1Password backend of a config section, a configured service account token skipping the prompt
func NewOnePasswordLoginBackend(ctx context.Context, cfg map[string]any) LoginBackend {
	return newOnePasswordLoginBackend(ctx, cfg)
}

func newOnePasswordLoginBackend(ctx context.Context, cfg map[string]any) *OnePasswordLoginBackend {
	if cfg == nil {
		cfg = map[string]any{}
	}
	b := &OnePasswordLoginBackend{ctx: ctx, cfg: cfg, itemVaults: map[string]string{}}
	b.Self = b
	envName := vaultText(cfg["service_account_token_env"])
	if envName == "" {
		envName = "OP_SERVICE_ACCOUNT_TOKEN"
	}
	b.serviceToken = vaultGetSecret(ctx, envName, "")
	b.run = b.runOp
	b.unlocked = func() bool { return b.serviceToken != "" || IsVaultUnlocked(b.ctx, b.Name()) }
	return b
}

func (*OnePasswordLoginBackend) Name() string        { return "onepassword" }
func (*OnePasswordLoginBackend) DisplayName() string { return "1Password" }
func (*OnePasswordLoginBackend) Prefix() string      { return "op:" }
func (*OnePasswordLoginBackend) NeedsUnlock() bool   { return true }
func (b *OnePasswordLoginBackend) IsUnlocked() bool  { return b.unlocked() }

func (b *OnePasswordLoginBackend) op() (string, error) {
	op := provider.FindOp(vaultText(b.cfg["binary_path"]))
	if op == "" {
		return "", errors.New("1Password CLI (op) not found — install it or set vault.onepassword.binary_path")
	}
	return pypath.PathStr(op), nil
}

// Allowlisted child environment, Connect credentials and the service token coming from the profile scope only
func (b *OnePasswordLoginBackend) env(sessionToken string) map[string]string {
	env := map[string]string{}
	for _, k := range provider.OpEnvAllowlist() {
		if v, ok := os.LookupEnv(k); ok && !strings.HasPrefix(k, "OP_CONNECT_") {
			env[k] = v
		}
	}
	for _, k := range []string{"OP_CONNECT_HOST", "OP_CONNECT_TOKEN"} {
		if v := vaultGetSecret(b.ctx, k, ""); v != "" {
			env[k] = v
		}
	}
	env["NO_COLOR"] = "1"
	account := vaultText(b.cfg["account"])
	if account != "" {
		env["OP_ACCOUNT"] = account
	}
	if b.serviceToken != "" {
		env["OP_SERVICE_ACCOUNT_TOKEN"] = b.serviceToken
	} else if sessionToken != "" {
		name := "OP_SESSION"
		if account != "" {
			name = "OP_SESSION_" + account
		}
		env[name] = sessionToken
	}
	return env
}

// Mints a session token from the master password consumed on stdin
func (b *OnePasswordLoginBackend) Unlock(masterPassword string) error {
	generation := BeginVaultUnlock(b.ctx, b.Name())
	op, err := b.op()
	if err != nil {
		return err
	}
	cmd := []string{op, "signin", "--raw"}
	if account := vaultText(b.cfg["account"]); account != "" {
		cmd = append(cmd, "--account", account)
	}
	proc, err := RunWithStdinSecret(cmd, b.env(""), masterPassword, vaultManagerTimeout, "op")
	if err != nil {
		return err
	}
	token := pystr.Strip(proc.Stdout)
	if proc.ReturnCode != 0 || token == "" {
		detail := vaultHead(provider.OpScrub(proc.Stderr), 200)
		if detail == "" {
			detail = "no session token"
		}
		return fmt.Errorf("1Password unlock failed: %s", detail)
	}
	if !StoreVaultSessionToken(b.ctx, b.Name(), token, &generation) {
		return errors.New("1Password was locked while unlocking; try again")
	}
	return nil
}

// Output of one op call, a session failure of a signed in account locking the backend again
func (b *OnePasswordLoginBackend) runOp(args ...string) (string, error) {
	token := ""
	if b.serviceToken == "" {
		token, _ = VaultSessionToken(b.ctx, b.Name())
		if token == "" {
			return "", &UnlockRequired{Backend: b}
		}
	}
	op, err := b.op()
	if err != nil {
		return "", err
	}
	proc, err := provider.RunCLI(append([]string{op}, args...), b.env(token), vaultManagerTimeout, "op", "op timed out", nil)
	if err != nil {
		return "", err
	}
	if proc.ReturnCode != 0 {
		detail := provider.OpScrub(proc.Stderr)
		lowered := strings.ToLower(detail)
		if b.serviceToken == "" && (strings.Contains(lowered, "session") || strings.Contains(lowered, "sign in") || strings.Contains(lowered, "not signed in")) {
			name := b.Name()
			LockVault(b.ctx, &name)
			return "", &UnlockRequired{Backend: b}
		}
		return "", fmt.Errorf("op failed: %s", vaultHead(detail, 200))
	}
	return proc.Stdout, nil
}

// Login metadata of every item with a usable origin, remembering each item's vault for later reads
func (b *OnePasswordLoginBackend) ListItems() ([]vault.VaultItemMeta, error) {
	if !b.IsUnlocked() {
		return []vault.VaultItemMeta{}, nil
	}
	text, err := b.run("item", "list", "--categories", "Login", "--format", "json")
	if err != nil {
		return nil, err
	}
	if text == "" {
		text = "[]"
	}
	raw, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	items, err := managerItems(raw)
	if err != nil {
		return nil, err
	}
	out := []vault.VaultItemMeta{}
	b.itemVaults = map[string]string{}
	for _, item := range items {
		if id := anyGet(item, "id"); pyrepr.Truthy(id) {
			vaultID := ""
			itemVault := anyGet(item, "vault")
			if !pyrepr.Truthy(itemVault) {
				itemVault = pyjson.NewObject()
			}
			if v, ok := pyjson.AsObject(itemVault); ok {
				vaultID = vaultText(anyGet(v, "id"))
			}
			b.itemVaults[pyrepr.Str(id)] = vaultID
		}
		urls := []string{}
		for _, u := range managerList(anyGet(item, "urls")) {
			if entry, ok := pyjson.AsObject(u); ok && pyrepr.Truthy(anyGet(entry, "href")) {
				urls = append(urls, pyrepr.Str(anyGet(entry, "href")))
			}
		}
		origins := allOrigins(urls)
		if len(origins) == 0 {
			continue
		}
		identifierType, identifier := managerIdentifier(anyGet(item, "additional_information"))
		label := vaultText(anyGet(item, "title"))
		if label == "" {
			label = origins[0]
		}
		out = append(out, vault.VaultItemMeta{ID: b.Prefix() + pyrepr.Str(anyGet(item, "id")), Kind: "login", Label: label,
			Origin: origins[0], CreatedAt: vaultText(anyGet(item, "created_at")), IdentifierType: identifierType,
			Identifier: identifier, AllowedOrigins: webOrigins(origins)})
	}
	return out, nil
}

func (b *OnePasswordLoginBackend) GetMeta(handle string) (*vault.VaultItemMeta, error) {
	items, err := b.ListItems()
	if err != nil {
		return nil, err
	}
	return findMeta(items, handle), nil
}

// The op item get target of a listed login naming the vault its listing reported
func (b *OnePasswordLoginBackend) itemSelector(handle string) ([]string, error) {
	itemID := handleTail(handle, b.Prefix())
	if _, listed := b.itemVaults[itemID]; !listed {
		if _, err := b.ListItems(); err != nil {
			return nil, err
		}
	}
	vaultID, listed := b.itemVaults[itemID]
	if !listed {
		return nil, errors.New("1Password login not found; list logins again")
	}
	if vaultID != "" {
		return []string{itemID, "--vault", vaultID}, nil
	}
	return []string{itemID}, nil
}

func (b *OnePasswordLoginBackend) ResolvePassword(handle string) (string, error) {
	selector, err := b.itemSelector(handle)
	if err != nil {
		return "", err
	}
	args := append(append([]string{"item", "get"}, selector...), "--fields", "label=password", "--reveal")
	out, err := b.run(args...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\r\n"), nil
}

// Current code of the item's one time password field, absent when op reports none
func (b *OnePasswordLoginBackend) ResolveOTP(handle string) (string, bool, error) {
	selector, err := b.itemSelector(handle)
	if err != nil {
		return "", false, nil
	}
	out, err := b.run(append(append([]string{"item", "get"}, selector...), "--otp")...)
	if err != nil {
		return "", false, nil
	}
	code := pystr.Strip(out)
	if !pystr.StrIsDigit(code) {
		return "", false, nil
	}
	return code, true, nil
}

var bitwardenEnvKeep = []string{"PATH", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot",
	"TMPDIR", "TMP", "TEMP", "XDG_CONFIG_HOME", "BITWARDENCLI_APPDATA_DIR"}

const bitwardenMasterEnv = "ICEBATH_BW_MASTER"

// Bitwarden Password Manager logins read through the bw CLI
type BitwardenLoginBackend struct {
	LoginBackendDefaults
	ctx      context.Context
	cfg      map[string]any
	run      func(args ...string) (string, error)
	unlocked func() bool
}

// Bitwarden backend of a config section
func NewBitwardenLoginBackend(ctx context.Context, cfg map[string]any) LoginBackend {
	return newBitwardenLoginBackend(ctx, cfg)
}

func newBitwardenLoginBackend(ctx context.Context, cfg map[string]any) *BitwardenLoginBackend {
	if cfg == nil {
		cfg = map[string]any{}
	}
	b := &BitwardenLoginBackend{ctx: ctx, cfg: cfg}
	b.Self = b
	b.run = b.runBw
	b.unlocked = func() bool { return IsVaultUnlocked(b.ctx, b.Name()) }
	return b
}

func (*BitwardenLoginBackend) Name() string        { return "bitwarden" }
func (*BitwardenLoginBackend) DisplayName() string { return "Bitwarden" }
func (*BitwardenLoginBackend) Prefix() string      { return "bw:" }
func (*BitwardenLoginBackend) NeedsUnlock() bool   { return true }
func (b *BitwardenLoginBackend) IsUnlocked() bool  { return b.unlocked() }

func (b *BitwardenLoginBackend) bw() (string, error) {
	found := vaultText(b.cfg["binary_path"])
	if found == "" {
		found, _ = exec.LookPath("bw")
	}
	if found == "" {
		return "", errors.New("Bitwarden CLI (bw) not found — install it or set vault.bitwarden.binary_path")
	}
	return pypath.PathStr(found), nil
}

func (b *BitwardenLoginBackend) env(sessionToken string) map[string]string {
	env := map[string]string{}
	for _, k := range bitwardenEnvKeep {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	env["NO_COLOR"] = "1"
	if sessionToken != "" {
		env["BW_SESSION"] = sessionToken
	}
	return env
}

// Mints a session key through the passwordenv contract, the variable set on the child environment only
func (b *BitwardenLoginBackend) Unlock(masterPassword string) error {
	generation := BeginVaultUnlock(b.ctx, b.Name())
	bw, err := b.bw()
	if err != nil {
		return err
	}
	proc, err := RunWithSecretEnv([]string{bw, "unlock", "--raw", "--nointeraction", "--passwordenv", bitwardenMasterEnv},
		b.env(""), bitwardenMasterEnv, masterPassword, vaultManagerTimeout, "bw")
	if err != nil {
		return err
	}
	token := pystr.Strip(proc.Stdout)
	if proc.ReturnCode != 0 || token == "" {
		detail := vaultHead(pystr.Strip(provider.ScrubANSI(proc.Stderr)), 200)
		if strings.Contains(strings.ToLower(detail), "not logged in") {
			detail = "not logged in — run `bw login` once in a terminal first"
		}
		if detail == "" {
			detail = "no session key"
		}
		return fmt.Errorf("Bitwarden unlock failed: %s", detail)
	}
	if !StoreVaultSessionToken(b.ctx, b.Name(), token, &generation) {
		return errors.New("Bitwarden was locked while unlocking; try again")
	}
	return nil
}

// Output of one bw call, a locked or expired session locking the backend again
func (b *BitwardenLoginBackend) runBw(args ...string) (string, error) {
	token, _ := VaultSessionToken(b.ctx, b.Name())
	if token == "" {
		return "", &UnlockRequired{Backend: b}
	}
	bw, err := b.bw()
	if err != nil {
		return "", err
	}
	argv := append(append([]string{bw}, args...), "--nointeraction")
	proc, err := provider.RunCLI(argv, b.env(token), vaultManagerTimeout, "bw", "bw timed out", nil)
	if err != nil {
		return "", err
	}
	if proc.ReturnCode != 0 {
		detail := provider.ScrubANSI(proc.Stderr)
		lowered := strings.ToLower(detail)
		if strings.Contains(lowered, "locked") || strings.Contains(lowered, "session") {
			name := b.Name()
			LockVault(b.ctx, &name)
			return "", &UnlockRequired{Backend: b}
		}
		return "", fmt.Errorf("bw failed: %s", vaultHead(detail, 200))
	}
	return proc.Stdout, nil
}

// Login metadata of every type 1 item with a fill target, URIs matched Never left out
func (b *BitwardenLoginBackend) ListItems() ([]vault.VaultItemMeta, error) {
	if !b.IsUnlocked() {
		return []vault.VaultItemMeta{}, nil
	}
	text, err := b.run("list", "items")
	if err != nil {
		return nil, err
	}
	if text == "" {
		text = "[]"
	}
	raw, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	items, err := managerItems(raw)
	if err != nil {
		return nil, err
	}
	out := []vault.VaultItemMeta{}
	for _, item := range items {
		login, isLogin := pyjson.AsObject(anyGet(item, "login"))
		if !pyrepr.Equal(anyGet(item, "type"), int64(1)) || !isLogin {
			continue
		}
		uris, err := managerItems(managerList(anyGet(login, "uris")))
		if err != nil {
			return nil, err
		}
		origins := []string{}
		for _, uri := range uris {
			if pyrepr.Equal(anyGet(uri, "match"), int64(5)) {
				continue
			}
			origin, err := vault.NormalizeOrigin(vaultText(anyGet(uri, "uri")))
			if err != nil {
				continue
			}
			if origin != "" && !hasToken(origins, origin) {
				origins = append(origins, origin)
			}
		}
		if len(origins) == 0 {
			continue
		}
		identifierType, identifier := managerIdentifier(anyGet(login, "username"))
		label := vaultText(anyGet(item, "name"))
		if label == "" {
			label = origins[0]
		}
		out = append(out, vault.VaultItemMeta{ID: b.Prefix() + pyrepr.Str(anyGet(item, "id")), Kind: "login", Label: label,
			Origin: origins[0], CreatedAt: vaultText(anyGet(item, "creationDate")), IdentifierType: identifierType,
			Identifier: identifier, AllowedOrigins: webOrigins(origins)})
	}
	return out, nil
}

func (b *BitwardenLoginBackend) GetMeta(handle string) (*vault.VaultItemMeta, error) {
	items, err := b.ListItems()
	if err != nil {
		return nil, err
	}
	return findMeta(items, handle), nil
}

func handleTail(handle, prefix string) string {
	r := []rune(handle)
	n := len([]rune(prefix))
	if len(r) <= n {
		return ""
	}
	return string(r[n:])
}

func (b *BitwardenLoginBackend) ResolvePassword(handle string) (string, error) {
	out, err := b.run("get", "password", handleTail(handle, b.Prefix()))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\r\n"), nil
}

// Current code of the item's TOTP seed, absent when bw reports none
func (b *BitwardenLoginBackend) ResolveOTP(handle string) (string, bool, error) {
	out, err := b.run("get", "totp", handleTail(handle, b.Prefix()))
	if err != nil {
		return "", false, nil
	}
	code := pystr.Strip(out)
	if !pystr.StrIsDigit(code) {
		return "", false, nil
	}
	return code, true, nil
}
