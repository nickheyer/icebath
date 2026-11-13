//go:build linux || freebsd || netbsd || openbsd || dragonfly

package safestorage

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	backendBasic     = "basic_text"
	backendLibsecret = "gnome_libsecret"
	backendKWallet   = "kwallet"
	backendKWallet5  = "kwallet5"
	backendKWallet6  = "kwallet6"

	chromiumKey    = "Chromium Safe Storage"
	chromiumFolder = "Chromium Keys"
	v10Password    = "peanuts"

	secretsDest     = "org.freedesktop.secrets"
	secretsPath     = "/org/freedesktop/secrets"
	secretsService  = "org.freedesktop.Secret.Service"
	secretsPrompt   = "org.freedesktop.Secret.Prompt"
	secretsDefault  = "/org/freedesktop/secrets/aliases/default"
	libsecretSchema = "chrome_libsecret_os_crypt_password_v2"
	kwalletIface    = "org.kde.KWallet"
)

type platformKeys struct {
	app     string
	backend string
	loaded  bool
	v10     []byte
	v11     []byte
}

func newPlatformKeys(o Options) *platformKeys {
	return &platformKeys{app: o.AppName, backend: selectBackend(o.PasswordStore, desktopEnvironment(os.LookupEnv))}
}

// Derives the fixed v10 key and the v11 key of the key store password once per process
func (k *platformKeys) load() {
	if k.loaded {
		return
	}
	k.loaded = true
	k.v10, _ = deriveCBCKey(v10Password, 1)
	var password string
	var ok bool
	switch k.backend {
	case backendLibsecret:
		password, ok = libsecretPassword(k.app)
	case backendKWallet:
		password, ok = kwalletPassword(4, k.app)
	case backendKWallet5:
		password, ok = kwalletPassword(5, k.app)
	case backendKWallet6:
		password, ok = kwalletPassword(6, k.app)
	}
	if ok {
		k.v11, _ = deriveCBCKey(password, 1)
	}
}

func (k *platformKeys) available(plain bool) bool {
	k.load()
	return k.v11 != nil || (plain && k.backend == backendBasic && k.v10 != nil)
}

func (k *platformKeys) encrypt(plaintext []byte) ([]byte, error) {
	if k.v11 != nil {
		return cbcSeal(k.v11, "v11", plaintext)
	}
	return cbcSeal(k.v10, "v10", plaintext)
}

func (k *platformKeys) decrypt(data []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(data, []byte("v10")):
		return cbcOpen(k.v10, data[3:])
	case bytes.HasPrefix(data, []byte("v11")):
		if k.v11 == nil {
			return nil, errors.New("no key store key")
		}
		return cbcOpen(k.v11, data[3:])
	}
	return data, nil
}

// Backend Chromium picks from an explicit --password-store or else the desktop environment
func selectBackend(store, desktop string) string {
	switch store {
	case "kwallet", "kwallet5", "kwallet6":
		return store
	case "gnome-libsecret":
		return backendLibsecret
	case "basic":
		return backendBasic
	}
	switch desktop {
	case "kde4":
		return backendKWallet
	case "kde5":
		return backendKWallet5
	case "kde6":
		return backendKWallet6
	case "cinnamon", "deepin", "gnome", "pantheon", "ukui", "unity", "xfce", "lxqt":
		return backendLibsecret
	}
	return backendBasic
}

// Desktop environment the way Chromium reads XDG_CURRENT_DESKTOP, DESKTOP_SESSION and older variables
func desktopEnvironment(lookup func(string) (string, bool)) string {
	if current, ok := lookup("XDG_CURRENT_DESKTOP"); ok {
		for _, value := range strings.Split(current, ":") {
			switch strings.TrimSpace(value) {
			case "Unity":
				if session, ok := lookup("DESKTOP_SESSION"); ok && strings.Contains(session, "gnome-fallback") {
					return "gnome"
				}
				return "unity"
			case "Deepin":
				return "deepin"
			case "GNOME":
				return "gnome"
			case "X-Cinnamon":
				return "cinnamon"
			case "KDE":
				if version, ok := lookup("KDE_SESSION_VERSION"); ok {
					switch version {
					case "5":
						return "kde5"
					case "6":
						return "kde6"
					}
				}
				return "kde4"
			case "Pantheon":
				return "pantheon"
			case "XFCE":
				return "xfce"
			case "UKUI":
				return "ukui"
			case "LXQt":
				return "lxqt"
			}
		}
	}
	if session, ok := lookup("DESKTOP_SESSION"); ok {
		switch {
		case session == "deepin":
			return "deepin"
		case session == "gnome" || session == "mate":
			return "gnome"
		case session == "kde4" || session == "kde-plasma":
			return "kde4"
		case session == "kde":
			if _, ok := lookup("KDE_SESSION_VERSION"); ok {
				return "kde4"
			}
			return "kde3"
		case strings.Contains(session, "xfce") || session == "xubuntu":
			return "xfce"
		case session == "ukui":
			return "ukui"
		}
	}
	if _, ok := lookup("GNOME_DESKTOP_SESSION_ID"); ok {
		return "gnome"
	}
	if _, ok := lookup("KDE_FULL_SESSION"); ok {
		if _, ok := lookup("KDE_SESSION_VERSION"); ok {
			return "kde4"
		}
		return "kde3"
	}
	return "other"
}

// Base64 of 16 random bytes, the password Chromium stores when the key store has none
func randomPassword() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

type secretValue struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

// Password of the app entry in the Secret Service default keyring, stored fresh when absent
func libsecretPassword(app string) (string, bool) {
	conn, err := dbus.SessionBusPrivate()
	if err != nil {
		return "", false
	}
	defer conn.Close()
	if conn.Auth(nil) != nil || conn.Hello() != nil {
		return "", false
	}
	svc := conn.Object(secretsDest, secretsPath)
	var output dbus.Variant
	var session dbus.ObjectPath
	if err := svc.Call(secretsService+".OpenSession", 0, "plain", dbus.MakeVariant("")).Store(&output, &session); err != nil {
		return "", false
	}
	ensureDefaultUnlocked(conn, svc)
	var unlocked, locked []dbus.ObjectPath
	if err := svc.Call(secretsService+".SearchItems", 0, map[string]string{"application": app}).Store(&unlocked, &locked); err != nil {
		return "", false
	}
	if len(locked) > 0 {
		unlocked = append(unlocked, unlockObjects(conn, svc, locked)...)
	}
	if len(unlocked) == 0 {
		return storeLibsecretPassword(conn, svc, session, app)
	}
	var secrets map[dbus.ObjectPath]secretValue
	if err := svc.Call(secretsService+".GetSecrets", 0, unlocked[:1], session).Store(&secrets); err != nil {
		return "", false
	}
	secret, ok := secrets[unlocked[0]]
	if !ok {
		return "", false
	}
	return string(secret.Value), true
}

// Unlocks the default keyring when it is locked, prompting the user the way libsecret does
func ensureDefaultUnlocked(conn *dbus.Conn, svc dbus.BusObject) {
	var collection dbus.ObjectPath
	if svc.Call(secretsService+".ReadAlias", 0, "default").Store(&collection) != nil || collection == "/" {
		return
	}
	prop, err := conn.Object(secretsDest, collection).GetProperty("org.freedesktop.Secret.Collection.Locked")
	if err != nil {
		return
	}
	if isLocked, _ := prop.Value().(bool); isLocked {
		unlockObjects(conn, svc, []dbus.ObjectPath{collection})
	}
}

// Objects the Secret Service unlocked, after its prompt when it asks for one
func unlockObjects(conn *dbus.Conn, svc dbus.BusObject, objects []dbus.ObjectPath) []dbus.ObjectPath {
	var unlocked []dbus.ObjectPath
	var prompt dbus.ObjectPath
	if svc.Call(secretsService+".Unlock", 0, objects).Store(&unlocked, &prompt) != nil {
		return nil
	}
	if prompt == "/" {
		return unlocked
	}
	result, ok := runPrompt(conn, prompt)
	if !ok {
		return unlocked
	}
	if paths, ok := result.Value().([]dbus.ObjectPath); ok {
		unlocked = append(unlocked, paths...)
	}
	return unlocked
}

// Runs a Secret Service prompt and waits for its Completed signal, false when dismissed
func runPrompt(conn *dbus.Conn, prompt dbus.ObjectPath) (dbus.Variant, bool) {
	match := []dbus.MatchOption{dbus.WithMatchObjectPath(prompt), dbus.WithMatchInterface(secretsPrompt), dbus.WithMatchMember("Completed")}
	if conn.AddMatchSignal(match...) != nil {
		return dbus.Variant{}, false
	}
	defer conn.RemoveMatchSignal(match...)
	signals := make(chan *dbus.Signal, 8)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)
	if conn.Object(secretsDest, prompt).Call(secretsPrompt+".Prompt", 0, "").Err != nil {
		return dbus.Variant{}, false
	}
	for signal := range signals {
		if signal.Path != prompt || signal.Name != secretsPrompt+".Completed" || len(signal.Body) < 2 {
			continue
		}
		if dismissed, _ := signal.Body[0].(bool); dismissed {
			return dbus.Variant{}, false
		}
		result, _ := signal.Body[1].(dbus.Variant)
		return result, true
	}
	return dbus.Variant{}, false
}

// Stores a random password under the Chromium schema and app attribute in the default keyring
func storeLibsecretPassword(conn *dbus.Conn, svc dbus.BusObject, session dbus.ObjectPath, app string) (string, bool) {
	password, err := randomPassword()
	if err != nil {
		return "", false
	}
	props := map[string]dbus.Variant{
		"org.freedesktop.Secret.Item.Label":      dbus.MakeVariant(chromiumKey),
		"org.freedesktop.Secret.Item.Attributes": dbus.MakeVariant(map[string]string{"xdg:schema": libsecretSchema, "application": app}),
	}
	secret := secretValue{Session: session, Parameters: []byte{}, Value: []byte(password), ContentType: "text/plain"}
	var item, prompt dbus.ObjectPath
	if conn.Object(secretsDest, secretsDefault).Call("org.freedesktop.Secret.Collection.CreateItem", 0, props, secret, true).Store(&item, &prompt) != nil {
		return "", false
	}
	if prompt != "/" {
		if _, ok := runPrompt(conn, prompt); !ok {
			return "", false
		}
	}
	return password, true
}

type walletState int

const (
	walletOK walletState = iota
	walletTemporaryFail
	walletPermanentFail
)

// Password of the Chromium entry in the network wallet of kwalletd, written fresh when absent or empty
func kwalletPassword(version int, app string) (string, bool) {
	conn, err := dbus.SessionBusPrivate()
	if err != nil {
		return "", false
	}
	defer conn.Close()
	if conn.Auth(nil) != nil || conn.Hello() != nil {
		return "", false
	}
	dest, path, daemon, launcher := "org.kde.kwalletd", "/modules/kwalletd", "kwalletd", "org.kde.klauncher"
	switch version {
	case 5:
		dest, path, daemon, launcher = "org.kde.kwalletd5", "/modules/kwalletd5", "kwalletd5", "org.kde.klauncher5"
	case 6:
		dest, path, daemon, launcher = "org.kde.kwalletd6", "/modules/kwalletd6", "kwalletd6", "org.kde.klauncher5"
	}
	wallet := conn.Object(dest, dbus.ObjectPath(path))
	name, state := initWallet(wallet)
	if state == walletTemporaryFail && startKWalletd(conn, launcher, daemon) {
		name, state = initWallet(wallet)
	}
	if state != walletOK {
		return "", false
	}
	var handle int32
	if wallet.Call(kwalletIface+".open", 0, name, int64(0), app).Store(&handle) != nil || handle == -1 {
		return "", false
	}
	defer wallet.Call(kwalletIface+".close", 0, handle, false, app)
	var hasFolder bool
	if wallet.Call(kwalletIface+".hasFolder", 0, handle, chromiumFolder, app).Store(&hasFolder) != nil {
		return "", false
	}
	if !hasFolder {
		var created bool
		if wallet.Call(kwalletIface+".createFolder", 0, handle, chromiumFolder, app).Store(&created) != nil || !created {
			return "", false
		}
	}
	var hasEntry bool
	if wallet.Call(kwalletIface+".hasEntry", 0, handle, chromiumFolder, chromiumKey, app).Store(&hasEntry) != nil {
		return "", false
	}
	if !hasEntry {
		return writeKWalletPassword(wallet, handle, app)
	}
	var entryType int32
	if wallet.Call(kwalletIface+".entryType", 0, handle, chromiumFolder, chromiumKey, app).Store(&entryType) != nil {
		return "", false
	}
	if entryType != 1 {
		var removed int32
		if wallet.Call(kwalletIface+".removeEntry", 0, handle, chromiumFolder, chromiumKey, app).Store(&removed) != nil || removed != 0 {
			return "", false
		}
		return writeKWalletPassword(wallet, handle, app)
	}
	var password string
	if wallet.Call(kwalletIface+".readPassword", 0, handle, chromiumFolder, chromiumKey, app).Store(&password) != nil {
		return "", false
	}
	if password == "" {
		return writeKWalletPassword(wallet, handle, app)
	}
	return password, true
}

// Enabled state and network wallet name, temporary when kwalletd cannot be reached
func initWallet(wallet dbus.BusObject) (string, walletState) {
	call := wallet.Call(kwalletIface+".isEnabled", 0)
	if call.Err != nil {
		return "", walletTemporaryFail
	}
	var enabled bool
	if call.Store(&enabled) != nil || !enabled {
		return "", walletPermanentFail
	}
	call = wallet.Call(kwalletIface+".networkWallet", 0)
	if call.Err != nil {
		return "", walletTemporaryFail
	}
	var name string
	if call.Store(&name) != nil {
		return "", walletPermanentFail
	}
	return name, walletOK
}

// Asks klauncher to start kwalletd, true when it reports no error
func startKWalletd(conn *dbus.Conn, launcher, daemon string) bool {
	var ret, pid int32
	var busName, failure string
	call := conn.Object(launcher, "/KLauncher").Call("org.kde.KLauncher.start_service_by_desktop_name", 0, daemon, []string{}, []string{}, "", false)
	if call.Store(&ret, &busName, &failure, &pid) != nil {
		return false
	}
	return failure == "" && ret == 0
}

// Writes a random password as the Chromium entry, the password when kwalletd accepted it
func writeKWalletPassword(wallet dbus.BusObject, handle int32, app string) (string, bool) {
	password, err := randomPassword()
	if err != nil {
		return "", false
	}
	var ret int32
	if wallet.Call(kwalletIface+".writePassword", 0, handle, chromiumFolder, chromiumKey, password, app).Store(&ret) != nil || ret != 0 {
		return "", false
	}
	return password, true
}
