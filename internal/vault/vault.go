// The autofill vault: logins, cards and addresses the agent may fill into web pages for the user, kept encrypted in the profile or read
// from the password managers plugins bring, the agent only ever seeing their handles and metadata
package vault

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	vaultconfig "github.com/nickheyer/icebath/internal/vault/config"
)

// Handles of the profile's own store begin with this
const localPrefix = "vault_"

// Name of the profile's own store as people read it
const OwnTitle = "icebath vault"

// A password manager the vault reads website logins from beside its own store
type Backend struct {
	// Name of its settings, vault.<name>, and of its handles, <name>:
	Name string
	// Name a person reads
	Title string
	// Whether its program is there for the profile
	Installed func(cfg *config.Config) bool
	// The manager for the profile
	Open func(cfg *config.Config) Manager
}

// What a password manager does for the vault; every call but Unlock carries the session token its Unlock answered, empty while Ready
type Manager interface {
	// Works now without an unlock, such as 1Password with a service account token
	Ready() bool
	// Signs in with the master password, answering the session token later calls carry
	Unlock(ctx context.Context, master string) (string, error)
	// Logins with a site, by their ids in the manager
	Items(ctx context.Context, session string) ([]Item, error)
	Password(ctx context.Context, session, id string) (string, error)
	// Current one time code of the login, false when it has none
	OTP(ctx context.Context, session, id string) (string, bool)
}

// A manager's answer that its session ended or never began, which locks it again
var ErrLocked = errors.New("The password manager is locked.")

// Login item of a password manager from what it keeps: every address saved on it as an origin, the first one its site and its label when
// it has no title, the web ones the sites it may be filled on; false for an item with no usable address
func Site(id, title, created, username string, urls []string) (Item, bool) {
	var origins []string
	for _, raw := range urls {
		if origin, err := NormalizeOrigin(raw); err == nil && !slices.Contains(origins, origin) {
			origins = append(origins, origin)
		}
	}
	if len(origins) == 0 {
		return Item{}, false
	}
	item := Item{ID: id, Kind: "login", Label: cmp.Or(strings.TrimSpace(title), origins[0]), Origin: origins[0], Created: created}
	if username = strings.TrimSpace(username); username != "" {
		item.IdentifierType, item.Identifier = "username", username
	}
	for _, origin := range origins {
		if strings.HasPrefix(origin, "http://") || strings.HasPrefix(origin, "https://") {
			item.Origins = append(item.Origins, origin)
		}
	}
	if len(item.Origins) == 0 {
		item.Origins = origins[:1]
	}
	return item, true
}

// One time code a manager printed, false when it printed something else
func Code(out string) (string, bool) {
	code := strings.TrimSpace(out)
	if code == "" || strings.ContainsFunc(code, func(r rune) bool { return r < '0' || r > '9' }) {
		return "", false
	}
	return code, true
}

// A password manager to unlock before it is read
type Locked struct{ Title string }

func (e *Locked) Error() string { return e.Title + " is locked. Unlock it in /vault first." }

type Registrant interface {
	Backends() []Backend
}

// Backends fixed once they are registered
type Backends []Backend

func (b Backends) Backends() []Backend { return b }

type Registry struct {
	registrants []Registrant
	// Host the registry is nested in, nil for the host's own
	host *Vault
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Backends() []Backend {
	var backends []Backend
	for _, registrant := range r.registrants {
		backends = append(backends, registrant.Backends()...)
	}
	return backends
}

// Host the registry is nested in
func (r *Registry) Host() *Vault {
	return r.host
}

// The vault of every profile and the password managers it reads
type Vault struct {
	Registry
	config   *config.Config
	tokens   *tokens
	once     sync.Once
	backends []Backend
}

func New(cfg *config.Config) *Vault {
	vaultconfig.Register(cfg)
	return &Vault{config: cfg, tokens: newTokens()}
}

// Adds a registrant; a registry nested in the host reaches it through Host
func (v *Vault) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = v
	}
	v.Registry.Register(registrant)
}

var backendName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Every password manager, checked once every plugin registered: a bad name, one the own store takes or one taken twice is a mistake of the
// code
func (v *Vault) all() []Backend {
	v.once.Do(func() {
		seen := map[string]bool{}
		for _, b := range v.Registry.Backends() {
			switch {
			case !backendName.MatchString(b.Name):
				panic(fmt.Sprintf("vault: password manager name %q is not a lower case identifier", b.Name))
			case b.Name == "local" || b.Name == "vault":
				panic("vault: password manager name " + b.Name + " is the profile's own store's")
			case seen[b.Name]:
				panic("vault: password manager " + b.Name + " is registered twice")
			case b.Open == nil:
				panic("vault: password manager " + b.Name + " opens no manager")
			}
			seen[b.Name] = true
			v.backends = append(v.backends, b)
		}
	})
	return v.backends
}

func (v *Vault) backend(name string) (Backend, bool) {
	for _, b := range v.all() {
		if b.Name == name {
			return b, true
		}
	}
	return Backend{}, false
}

// The profile's own encrypted vault
func (v *Vault) Store(cfg *config.Config) *Store {
	storage := strings.ToLower(strings.TrimSpace(cfg.GetString(vaultconfig.KeyStorage)))
	return &Store{dir: filepath.Join(cfg.ProfileDir(cfg.Profile()), "vault"), storage: storage}
}

func (b Backend) installed(cfg *config.Config) bool {
	return b.Installed == nil || b.Installed(cfg)
}

// Whether the profile reads logins from the manager: its program is there and the profile did not turn it off
func (v *Vault) enabled(cfg *config.Config, b Backend) bool {
	return b.installed(cfg) && cfg.GetBool(vaultconfig.Of(b.Name, vaultconfig.Enabled))
}

// A source of logins for a profile: its own store, or a password manager it reads
type Login struct {
	Name, Title string
	vault       *Vault
	cfg         *config.Config
	// Nil for the profile's own store
	manager Manager
}

// Sources of logins of the profile: its own store first, then every password manager it reads
func (v *Vault) Logins(cfg *config.Config) []Login {
	out := []Login{{Name: "local", Title: OwnTitle, vault: v, cfg: cfg}}
	for _, b := range v.all() {
		if v.enabled(cfg, b) {
			out = append(out, Login{Name: b.Name, Title: b.Title, vault: v, cfg: cfg, manager: b.Open(cfg)})
		}
	}
	return out
}

// Source of logins the handle is of, false when no source the profile reads owns it
func (v *Vault) Login(cfg *config.Config, handle string) (Login, bool) {
	for _, login := range v.Logins(cfg) {
		if login.Owns(handle) {
			return login, true
		}
	}
	return Login{}, false
}

// Beginning of every handle of the source
func (l Login) Prefix() string {
	if l.manager == nil {
		return localPrefix
	}
	return l.Name + ":"
}

func (l Login) Owns(handle string) bool { return strings.HasPrefix(handle, l.Prefix()) }

// The profile's own store, whose items can be removed here
func (l Login) Local() bool { return l.manager == nil }

// Unlocked with the user's master password before it is read
func (l Login) NeedsUnlock() bool { return l.manager != nil }

func (l Login) key() tokenKey { return tokenKey{l.cfg.Profile(), l.Name} }

func (l Login) Unlocked() bool {
	if l.manager == nil || l.manager.Ready() {
		return true
	}
	_, ok := l.vault.tokens.live(l.key(), false)
	return ok
}

// Session token of a real call to the manager, Locked when it waits for an unlock
func (l Login) session() (string, error) {
	if l.manager.Ready() {
		return "", nil
	}
	if value, ok := l.vault.tokens.live(l.key(), true); ok {
		return value, nil
	}
	return "", &Locked{Title: l.Title}
}

// A failure of the manager, its locking again said as Locked and the token forgotten
func (l Login) failed(err error) error {
	if errors.Is(err, ErrLocked) {
		l.vault.tokens.lock(l.cfg.Profile(), l.Name)
		return &Locked{Title: l.Title}
	}
	return err
}

// Unlocks the manager with the master password, for the session given or, with none, until it goes idle or is locked
func (l Login) Unlock(ctx context.Context, master, session string) error {
	if l.manager == nil {
		return nil
	}
	generation := l.vault.tokens.begin(l.key())
	value, err := l.manager.Unlock(ctx, master)
	if err != nil {
		return err
	}
	if !l.vault.tokens.commit(l.key(), value, generation, session) {
		return fmt.Errorf("%s was locked again while it was unlocking. Unlock it again.", l.Title)
	}
	return nil
}

// Every item of the source, none of a manager waiting for an unlock
func (l Login) Items(ctx context.Context) ([]Item, error) {
	if l.manager == nil {
		return l.vault.Store(l.cfg).Items()
	}
	if !l.Unlocked() {
		return []Item{}, nil
	}
	session, err := l.session()
	if err != nil {
		return nil, err
	}
	got, err := l.manager.Items(ctx, session)
	if err != nil {
		return nil, l.failed(err)
	}
	out := make([]Item, 0, len(got))
	for _, item := range got {
		if slices.Contains(Kinds, item.Kind) {
			item.ID = l.Prefix() + item.ID
			out = append(out, item)
		}
	}
	return out, nil
}

// Metadata of the item, nil when the source has none of the handle
func (l Login) Item(ctx context.Context, handle string) (*Item, error) {
	if !l.Owns(handle) {
		return nil, nil
	}
	if l.manager == nil {
		return l.vault.Store(l.cfg).Item(handle)
	}
	items, err := l.Items(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].ID == handle {
			return &items[i], nil
		}
	}
	return nil, nil
}

func (l Login) id(handle string) (string, error) {
	if !l.Owns(handle) {
		return "", &Error{fmt.Sprintf("%s is not the handle of an item in %s.", handle, l.Title)}
	}
	return strings.TrimPrefix(handle, l.Prefix()), nil
}

// Password of the login, for a fill on the server only
func (l Login) Password(ctx context.Context, handle string) (string, error) {
	if l.manager == nil {
		secret, err := l.vault.Store(l.cfg).Secret(handle)
		if err != nil {
			return "", err
		}
		return secret["password"], nil
	}
	id, err := l.id(handle)
	if err != nil {
		return "", err
	}
	session, err := l.session()
	if err != nil {
		return "", err
	}
	password, err := l.manager.Password(ctx, session, id)
	if err != nil {
		return "", l.failed(err)
	}
	return password, nil
}

// Current one time code of the login, false when it keeps no authenticator key
func (l Login) OTP(ctx context.Context, handle string) (string, bool, error) {
	if l.manager == nil {
		secret, err := l.vault.Store(l.cfg).Secret(handle)
		if err != nil {
			return "", false, err
		}
		if secret["otp_secret"] == "" {
			return "", false, nil
		}
		code, err := totp(secret["otp_secret"], time.Now())
		return code, err == nil, err
	}
	id, err := l.id(handle)
	if err != nil {
		return "", false, err
	}
	session, err := l.session()
	if err != nil {
		return "", false, err
	}
	code, ok := l.manager.OTP(ctx, session, id)
	return code, ok, nil
}

// Every value of the item by field, for a fill on the server only: a password manager's login is its password alone
func (l Login) Secret(ctx context.Context, handle string) (map[string]string, error) {
	if l.manager == nil {
		return l.vault.Store(l.cfg).Secret(handle)
	}
	password, err := l.Password(ctx, handle)
	if err != nil {
		return nil, err
	}
	return map[string]string{"password": password}, nil
}

// Unlocks the password manager of the name for the profile with its master password, for the session given or, with none, until idle
func (v *Vault) Unlock(ctx context.Context, cfg *config.Config, name, master, session string) error {
	for _, login := range v.Logins(cfg) {
		if login.Name == name && login.NeedsUnlock() {
			return login.Unlock(ctx, master, session)
		}
	}
	return &Error{"There is no password manager named " + name + " to unlock. Turn it on in /settings under Autofill vault."}
}

// Locks the password manager of the name for the profile, every one for no name
func (v *Vault) Lock(cfg *config.Config, name string) {
	v.tokens.lock(cfg.Profile(), name)
}

// Locks what the ended session unlocked
func (v *Vault) Release(_ context.Context, session string) {
	v.tokens.release(session)
}

// Locks every password manager of the deleted profile
func (v *Vault) Delete(profile string) error {
	v.tokens.lock(profile, "")
	return nil
}

// Locks every password manager of a profile before its folder moves to a new name
func (v *Vault) Stop(profile string) error {
	v.tokens.lock(profile, "")
	return nil
}

// Keeps the keychain entry of a renamed profile's vault its own: the entry named the vault's old folder, which moved with the profile
func (v *Vault) Rename(old, renamed string) error {
	store := v.Store(v.config.In(renamed))
	ref, err := readRef(store.refPath())
	if err != nil || ref == nil {
		return err
	}
	parent := filepath.Dir(v.config.ProfileDir(old))
	if ref.Dir != filepath.Join(resolved(parent), filepath.Base(v.config.ProfileDir(old)), "vault") {
		return nil
	}
	ref.Dir = resolved(store.dir)
	return writeRef(store.refPath(), ref)
}

// Locks every password manager as icebath stops
func (v *Vault) Close() error {
	v.tokens.close()
	return nil
}

// One item listed with the source it comes from
type Listed struct {
	Item
	Source string
	Local  bool
}

// Items of every source the profile reads that is unlocked, its own first, and the names of the password managers waiting to be unlocked
func (v *Vault) Listing(ctx context.Context, cfg *config.Config) ([]Listed, []string, error) {
	var rows []Listed
	var locked []string
	for _, login := range v.Logins(cfg) {
		if login.NeedsUnlock() && !login.Unlocked() {
			locked = append(locked, login.Title)
			continue
		}
		items, err := login.Items(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, item := range items {
			rows = append(rows, Listed{Item: item, Source: login.Title, Local: login.Local()})
		}
	}
	return rows, locked, nil
}

// One password manager and whether the profile reads it: on, off for one turned off, missing for one whose program is not there
type Source struct {
	Name, Title, State string
	// Waits to be unlocked
	Locked bool
}

func (v *Vault) Sources(cfg *config.Config) []Source {
	var out []Source
	for _, b := range v.all() {
		source := Source{Name: b.Name, Title: b.Title, State: "on"}
		switch {
		case !b.installed(cfg):
			source.State = "missing"
		case !v.enabled(cfg, b):
			source.State = "off"
		default:
			login := Login{Name: b.Name, Title: b.Title, vault: v, cfg: cfg, manager: b.Open(cfg)}
			source.Locked = !login.Unlocked()
		}
		out = append(out, source)
	}
	return out
}

// Turns a password manager on or off for the profile's logins, on being the absence of the opt out
func (v *Vault) SetSource(cfg *config.Config, name string, on bool) (Backend, error) {
	b, ok := v.backend(name)
	if !ok {
		names := make([]string, len(v.all()))
		for i, known := range v.all() {
			names[i] = known.Name
		}
		return Backend{}, &Error{fmt.Sprintf("icebath knows no password manager named %q. It knows %s.", name, strings.Join(names, ", "))}
	}
	var value any
	if !on {
		value = false
	}
	if err := cfg.Set(vaultconfig.Of(name, vaultconfig.Enabled), value); err != nil {
		return Backend{}, err
	}
	if !on {
		v.tokens.lock(cfg.Profile(), name)
	}
	return b, nil
}

// One field of an item of a kind besides its label and site, in the order they are asked
type Field struct {
	Key              string
	Required, Secret bool
	// Values it takes, nil for free text
	Choices []string
}

// Fields of an item of the kind, nil for a kind the vault does not keep
func KindFields(kind string) []Field {
	switch kind {
	case "login":
		return []Field{{Key: "identifier_type", Required: true, Choices: slices.Clone(IdentifierTypes)}, {Key: "identifier", Required: true},
			{Key: "password", Required: true, Secret: true}, {Key: "otp_secret", Secret: true}}
	case "payment", "address":
		fields := AddressFields
		if kind == "payment" {
			fields = PaymentFields
		}
		out := make([]Field, len(fields))
		for i, f := range fields {
			out[i] = Field{Key: f[0], Required: slices.Contains(required[kind], f[0]), Secret: kind == "payment"}
		}
		return out
	}
	return nil
}

// Saves an item of a kind in the profile's own vault from its values by field, the fields left empty staying out
func (v *Vault) Add(cfg *config.Config, kind, label, origin string, values map[string]string) (Item, error) {
	fields := KindFields(kind)
	if fields == nil {
		return Item{}, &Error{fmt.Sprintf("The vault keeps logins, payment cards and addresses, not “%s”.", kind)}
	}
	secret := map[string]string{}
	for _, field := range fields {
		value := values[field.Key]
		if field.Required && strings.TrimSpace(value) == "" {
			return Item{}, &Error{"Fill in the " + FieldName(field.Key) + "."}
		}
		if strings.TrimSpace(value) != "" {
			secret[field.Key] = value
		}
	}
	return v.Store(cfg).Add(kind, strings.TrimSpace(label), strings.TrimSpace(origin), secret)
}

// Where the key of the profile's own vault lives, where a new one goes and the keychain of this computer
type KeyView struct {
	Location
	Configured, Keychain string
}

func (v *Vault) Key(cfg *config.Config) (KeyView, error) {
	where, err := v.Store(cfg).Location()
	if err != nil {
		return KeyView{}, err
	}
	return KeyView{Location: where, Configured: v.Store(cfg).storage, Keychain: KeychainLabel()}, nil
}

// Moves the key of the profile's own vault to the keychain or back to its file, keeping the choice for new vaults, and answers where it
// lives after it
func (v *Vault) MoveKey(cfg *config.Config, target string) (KeyView, error) {
	if target != "keychain" && target != "file" {
		return KeyView{}, &Error{"The vault key can be kept in the keychain or in a file, not in “" + target + "”."}
	}
	if err := v.Store(cfg).Migrate(target); err != nil {
		return KeyView{}, err
	}
	if err := cfg.Set(vaultconfig.KeyStorage, target); err != nil {
		return KeyView{}, err
	}
	return v.Key(cfg)
}
