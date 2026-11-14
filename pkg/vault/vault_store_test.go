package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyfernet"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type memoryKeyring struct {
	mu        sync.Mutex
	entries   map[[2]string]string
	forgetful bool
}

func (*memoryKeyring) keyringModule() string { return "tests.agent.test_vault_key_storage" }
func (*memoryKeyring) keyringClass() string  { return "MemoryKeyring" }
func (k *memoryKeyring) GetPassword(service, account string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.forgetful {
		return "bm90LXRoZS1rZXktbm90LXRoZS1rZXktbm90LXRoZS1rZXk=", true, nil
	}
	v, ok := k.entries[[2]string{service, account}]
	return v, ok, nil
}
func (k *memoryKeyring) SetPassword(service, account, password string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.entries[[2]string{service, account}] = password
	return nil
}
func (k *memoryKeyring) DeletePassword(service, account string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.entries[[2]string{service, account}]; !ok {
		return &keyringError{"PasswordDeleteError", "no such entry"}
	}
	delete(k.entries, [2]string{service, account})
	return nil
}
func (k *memoryKeyring) count() int { k.mu.Lock(); defer k.mu.Unlock(); return len(k.entries) }

func useKeyring(t *testing.T, b keyringBackend, err error) {
	prev := keyringGetKeyring
	keyringGetKeyring = func() (keyringBackend, error) { return b, err }
	t.Cleanup(func() { keyringGetKeyring = prev })
}

func installMemoryKeyring(t *testing.T) *memoryKeyring {
	k := &memoryKeyring{entries: map[[2]string]string{}}
	useKeyring(t, k, nil)
	return k
}

// Temp home under root with an optional vault.key_storage, made the home in effect
func vaultHome(t *testing.T, root, name, keyStorage string) string {
	h := filepath.Join(root, name)
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	if keyStorage != "" {
		os.WriteFile(filepath.Join(h, "config.yaml"), []byte("vault:\n  key_storage: "+keyStorage+"\n"), 0o600)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func testLogin() *pyjson.Object {
	return pyjson.ObjectOf("identifier_type", "email", "identifier", "jane@example.com", "password", "dummy-pw-not-real-42")
}

func addLogin(t *testing.T, s *VaultStore) string {
	m, err := s.AddItem("login", "Example", testLogin(), "https://example.com")
	if err != nil {
		t.Fatalf("add %v", err)
	}
	return m.ID
}

func mustPassword(t *testing.T, s *VaultStore, id string) {
	t.Helper()
	sec, err := s.ResolveSecret(id)
	if err != nil {
		t.Fatalf("resolve %v", err)
	}
	if v, _ := sec.Get("password"); v != "dummy-pw-not-real-42" {
		t.Fatalf("password %v", v)
	}
}

func vaultErrContains(t *testing.T, err error, want string) {
	t.Helper()
	var ve *VaultError
	if !errors.As(err, &ve) || !strings.Contains(ve.Msg, want) {
		t.Fatalf("want VaultError containing %q, got %v", want, err)
	}
}

func cur() *VaultStore { return NewVaultStore(context.Background(), "") }

func TestKeychainModePutsANewVaultKeyInTheKeychainPerProfile(t *testing.T) {
	kc := installMemoryKeyring(t)
	root := t.TempDir()
	a := vaultHome(t, root, "a", "keychain")
	itemA := addLogin(t, cur())
	if pathExists(filepath.Join(a, "vault", "vault.key")) {
		t.Fatal("key file beside keychain ciphertext")
	}
	if st, _ := os.Stat(filepath.Join(a, "vault", "vault.json.enc")); st == nil || st.Size() == 0 {
		t.Fatal("no ciphertext")
	}
	if kc.count() != 1 {
		t.Fatalf("entries %d", kc.count())
	}
	mustPassword(t, cur(), itemA)
	b := vaultHome(t, root, "b", "keychain")
	itemB := addLogin(t, cur())
	if pathExists(filepath.Join(b, "vault", "vault.key")) || kc.count() != 2 {
		t.Fatalf("each profile gets its own keychain entry, %d", kc.count())
	}
	mustPassword(t, cur(), itemB)
	mustPassword(t, NewVaultStore(context.Background(), filepath.Join(a, "vault")), itemA)
}

func TestMigrationWritesKeychainVerifiesThenRemovesTheFileAndBack(t *testing.T) {
	kc := installMemoryKeyring(t)
	h := vaultHome(t, t.TempDir(), "default", "")
	store := cur()
	item := addLogin(t, store)
	keyFile := filepath.Join(h, "vault", "vault.key")
	oldKey, _ := readKeyFile(keyFile)
	if got, err := store.MigrateKey("keychain"); got != "keychain" || err != nil {
		t.Fatalf("to keychain %q %v", got, err)
	}
	if pathExists(keyFile) || kc.count() != 1 {
		t.Fatal("key file kept after the move")
	}
	for _, v := range kc.entries {
		if v != string(oldKey) {
			t.Fatalf("entry %q", v)
		}
	}
	mustPassword(t, cur(), item)
	if got, err := store.MigrateKey("file"); got != "file" || err != nil {
		t.Fatalf("to file %q %v", got, err)
	}
	back, _ := readKeyFile(keyFile)
	if string(back) != string(oldKey) || kc.count() != 0 {
		t.Fatalf("file %q entries %d", back, kc.count())
	}
	if st, _ := os.Stat(keyFile); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	mustPassword(t, cur(), item)
}

func TestMigrationKeepsTheKeyFileWhenTheKeychainCannotReturnTheKey(t *testing.T) {
	h := vaultHome(t, t.TempDir(), "default", "")
	useKeyring(t, &memoryKeyring{entries: map[[2]string]string{}, forgetful: true}, nil)
	store := cur()
	item := addLogin(t, store)
	_, err := store.MigrateKey("keychain")
	vaultErrContains(t, err, "keychain")
	if !pathExists(filepath.Join(h, "vault", "vault.key")) || pathExists(filepath.Join(h, "vault", "vault.keyref")) {
		t.Fatal("files changed")
	}
	mustPassword(t, cur(), item)
}

func TestMissingKeyWithAnExistingVaultExplainsRecoveryAndMintsNothing(t *testing.T) {
	for _, lose := range []string{"keychain entry", "key file"} {
		t.Run(lose, func(t *testing.T) {
			kc := installMemoryKeyring(t)
			storage := ""
			if lose == "keychain entry" {
				storage = "keychain"
			}
			h := vaultHome(t, t.TempDir(), "default", storage)
			addLogin(t, cur())
			blob, _ := os.ReadFile(filepath.Join(h, "vault", "vault.json.enc"))
			if lose == "keychain entry" {
				kc.entries = map[[2]string]string{}
			} else {
				os.Remove(filepath.Join(h, "vault", "vault.key"))
			}
			_, err := cur().ListItems()
			vaultErrContains(t, err, "icebath vault key")
			vaultErrContains(t, err, "vault.json.enc")
			after, _ := os.ReadFile(filepath.Join(h, "vault", "vault.json.enc"))
			if string(after) != string(blob) {
				t.Fatal("the ciphertext must be left alone")
			}
			if pathExists(filepath.Join(h, "vault", "vault.key")) || kc.count() != 0 {
				t.Fatal("no fresh key may be minted over an existing vault")
			}
			if lose == "keychain entry" {
				_, err := cur().MigrateKey("keychain")
				vaultErrContains(t, err, "icebath vault key")
			}
		})
	}
}

func TestKeychainModeWithoutAReachableKeychainFallsBackToTheKeyFile(t *testing.T) {
	for _, headless := range []string{"no secret service", "backend discovery raised"} {
		t.Run(headless, func(t *testing.T) {
			h := vaultHome(t, t.TempDir(), "default", "keychain")
			if headless == "no secret service" {
				useKeyring(t, failKeyring{}, nil)
			} else {
				useKeyring(t, nil, &keyringError{"RuntimeError", "entry point raised"})
			}
			item := addLogin(t, cur())
			keyFile := filepath.Join(h, "vault", "vault.key")
			st, err := os.Stat(keyFile)
			if err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
				t.Fatalf("key file %v", err)
			}
			if pathExists(filepath.Join(h, "vault", "vault.keyref")) {
				t.Fatal("pointer without a keychain")
			}
			mustPassword(t, cur(), item)
			_, err = cur().MigrateKey("keychain")
			vaultErrContains(t, err, "keychain")
			if !pathExists(keyFile) {
				t.Fatal("key file lost")
			}
		})
	}
}

func TestAStrayKeyFileThatCannotOpenTheVaultNeverCostsTheKeychainCopy(t *testing.T) {
	kc := installMemoryKeyring(t)
	h := vaultHome(t, t.TempDir(), "default", "keychain")
	item := addLogin(t, cur())
	os.WriteFile(filepath.Join(h, "vault", "vault.key"), pyfernet.GenerateKey(), 0o600)
	good := map[[2]string]string{}
	for k, v := range kc.entries {
		good[k] = v
	}
	for _, target := range []string{"file", "keychain"} {
		_, err := cur().MigrateKey(target)
		vaultErrContains(t, err, "does not decrypt")
	}
	if len(kc.entries) != len(good) {
		t.Fatal("keychain copy lost")
	}
	for k, v := range good {
		if kc.entries[k] != v {
			t.Fatal("keychain copy changed")
		}
	}
	os.Remove(filepath.Join(h, "vault", "vault.key"))
	mustPassword(t, cur(), item)
}

func TestClonedAndRenamedKeychainVaultsNeverShareOrOverwriteAnEntry(t *testing.T) {
	installMemoryKeyring(t)
	root := t.TempDir()
	work := vaultHome(t, root, "work", "keychain")
	item := addLogin(t, cur())
	a := vaultHome(t, root, "clone-a", "keychain")
	b := vaultHome(t, root, "clone-b", "keychain")
	for _, clone := range []string{a, b} {
		if err := os.CopyFS(filepath.Join(clone, "vault"), os.DirFS(filepath.Join(work, "vault"))); err != nil {
			t.Fatal(err)
		}
	}
	at := func(dir string) *VaultStore { return NewVaultStore(context.Background(), filepath.Join(dir, "vault")) }
	for _, step := range []struct{ dir, target string }{{a, "file"}, {b, "keychain"}, {work, "file"}} {
		if got, err := at(step.dir).MigrateKey(step.target); got != step.target || err != nil {
			t.Fatalf("%s %s %q %v", step.dir, step.target, got, err)
		}
	}
	for _, dir := range []string{a, b, work} {
		mustPassword(t, at(dir), item)
	}
	if got, err := at(work).MigrateKey("keychain"); got != "keychain" || err != nil {
		t.Fatalf("back %q %v", got, err)
	}
	renamed := vaultHome(t, root, "renamed", "keychain")
	if err := os.Rename(filepath.Join(work, "vault"), filepath.Join(renamed, "vault")); err != nil {
		t.Fatal(err)
	}
	addLogin(t, at(work))
	mustPassword(t, at(renamed), item)
}

func TestMigrationWhosePointerCannotBeSavedLeavesNoUntrackedEntry(t *testing.T) {
	kc := installMemoryKeyring(t)
	h := vaultHome(t, t.TempDir(), "default", "")
	item := addLogin(t, cur())
	prev := vaultWriteRef
	vaultWriteRef = func(_ context.Context, path string, ref *pyjson.Object) error {
		return &os.PathError{Op: "write", Path: path, Err: syscall.ENOSPC}
	}
	t.Cleanup(func() { vaultWriteRef = prev })
	_, err := cur().MigrateKey("keychain")
	vaultErrContains(t, err, "vault.keyref")
	if kc.count() != 0 {
		t.Fatal("a retry would mint another random account, so nothing may be left")
	}
	if !pathExists(filepath.Join(h, "vault", "vault.key")) {
		t.Fatal("key file lost")
	}
	mustPassword(t, cur(), item)
}

func TestCopiedVaultKeepsOpeningWhenSavingItsOwnPointerFails(t *testing.T) {
	for _, landed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pointer_not_landed", true: "pointer_landed"}[landed], func(t *testing.T) {
			kc := installMemoryKeyring(t)
			root := t.TempDir()
			work := vaultHome(t, root, "work", "keychain")
			item := addLogin(t, cur())
			clone := vaultHome(t, root, "clone", "keychain")
			if err := os.CopyFS(filepath.Join(clone, "vault"), os.DirFS(filepath.Join(work, "vault"))); err != nil {
				t.Fatal(err)
			}
			real := vaultWriteRef
			vaultWriteRef = func(ctx context.Context, path string, ref *pyjson.Object) error {
				if landed {
					real(ctx, path, ref)
				}
				return &os.PathError{Op: "write", Path: path, Err: syscall.ENOSPC}
			}
			t.Cleanup(func() { vaultWriteRef = real })
			mustPassword(t, cur(), item)
			want := 1
			if landed {
				want = 2
			}
			if kc.count() != want {
				t.Fatalf("entries %d want %d", kc.count(), want)
			}
			vaultWriteRef = real
			mustPassword(t, cur(), item)
		})
	}
}

func TestCleanupOfAnAlreadyDeletedKeychainEntryCompletes(t *testing.T) {
	installMemoryKeyring(t)
	h := vaultHome(t, t.TempDir(), "default", "keychain")
	item := addLogin(t, cur())
	keyref, _ := os.ReadFile(filepath.Join(h, "vault", "vault.keyref"))
	if got, err := cur().MigrateKey("file"); got != "file" || err != nil {
		t.Fatalf("%q %v", got, err)
	}
	os.WriteFile(filepath.Join(h, "vault", "vault.keyref"), keyref, 0o600)
	if got, err := cur().MigrateKey("file"); got != "file" || err != nil {
		t.Fatalf("rerun %q %v", got, err)
	}
	loc, err := cur().KeyLocation()
	if err != nil {
		t.Fatal(err)
	}
	if _, left := loc.Get("keychain_entry_left"); left || pathExists(filepath.Join(h, "vault", "vault.keyref")) {
		t.Fatal("pointer kept")
	}
	mustPassword(t, cur(), item)
}

func TestCorruptVaultRaisesVaultErrorOnEveryReader(t *testing.T) {
	ops := map[string]func(s *VaultStore) error{
		"list_items":     func(s *VaultStore) error { _, err := s.ListItems(); return err },
		"get_meta":       func(s *VaultStore) error { _, err := s.GetMeta("vault_x"); return err },
		"remove_item":    func(s *VaultStore) error { _, err := s.RemoveItem("vault_x"); return err },
		"resolve_secret": func(s *VaultStore) error { _, err := s.ResolveSecret("vault_x"); return err },
		"add_item": func(s *VaultStore) error {
			_, err := s.AddItem("login", "x", pyjson.ObjectOf("identifier_type", "email", "identifier", "u", "password", "p"), "https://x.example")
			return err
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			s := NewVaultStore(context.Background(), filepath.Join(t.TempDir(), "vault"))
			if _, err := s.AddItem("login", "Example login", pyjson.ObjectOf("identifier_type", "email", "identifier", "u@example.com", "password", "pw"), "https://example.com"); err != nil {
				t.Fatal(err)
			}
			f, err := s.fernet()
			if err != nil {
				t.Fatal(err)
			}
			os.WriteFile(s.vaultPath, f.Encrypt([]byte("{not json")), 0o600)
			var ve *VaultError
			if err := op(s); !errors.As(err, &ve) {
				t.Fatalf("want VaultError, got %v", err)
			}
		})
	}
}

// Real keychain round trips run only on macOS and Windows so a developer Secret Service is never touched
func TestRealKeychainStoresReadsAndDeletesADummyEntry(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("platforms macos and windows")
	}
	if _, err := osKeyring(); err != nil {
		t.Skipf("no OS keychain on this host: %v", err)
	}
	ref := pyjson.ObjectOf("storage", "keychain", "service", VaultKeychainService, "account", "icebath-ci-smoke-"+filepath.Base(t.TempDir()))
	t.Cleanup(func() { vaultDeleteKey(ref) })
	key := pyfernet.GenerateKey()
	if err := vaultStoreKey(ref, key); err != nil {
		t.Fatal(err)
	}
	if got, err := vaultLoadKey(ref); err != nil || string(got) != string(key) {
		t.Fatalf("load %q %v", got, err)
	}
	if !vaultDeleteKey(ref) {
		t.Fatal("delete")
	}
	if got, err := vaultLoadKey(ref); got != nil || err != nil {
		t.Fatalf("after delete %q %v", got, err)
	}
}

func TestKeychainModeVaultRoundTripsThroughTheRealKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("platforms macos and windows")
	}
	if _, err := osKeyring(); err != nil {
		t.Skipf("no OS keychain on this host: %v", err)
	}
	h := vaultHome(t, t.TempDir(), "icebath-home", "keychain")
	vault := filepath.Join(h, "vault")
	var made []*pyjson.Object
	t.Cleanup(func() {
		for _, r := range made {
			vaultDeleteKey(r)
		}
	})
	item := addLogin(t, cur())
	ref, err := vaultReadRef(filepath.Join(vault, "vault.keyref"))
	if err != nil || ref == nil {
		t.Fatalf("keychain mode must keep only a pointer beside the ciphertext %v", err)
	}
	made = append(made, ref)
	if pathExists(filepath.Join(vault, "vault.key")) {
		t.Fatal("key file")
	}
	mustPassword(t, cur(), item)
	if got, err := cur().MigrateKey("file"); got != "file" || err != nil {
		t.Fatalf("%q %v", got, err)
	}
	if !pathExists(filepath.Join(vault, "vault.key")) || pathExists(filepath.Join(vault, "vault.keyref")) {
		t.Fatal("layout after move to file")
	}
	if got, _ := vaultLoadKey(ref); got != nil {
		t.Fatal("moving to the file deletes the vault's own keychain entry")
	}
	if got, err := cur().MigrateKey("keychain"); got != "keychain" || err != nil {
		t.Fatalf("%q %v", got, err)
	}
	back, _ := vaultReadRef(filepath.Join(vault, "vault.keyref"))
	if back != nil {
		made = append(made, back)
	}
	if pathExists(filepath.Join(vault, "vault.key")) {
		t.Fatal("key file after move back")
	}
	mustPassword(t, cur(), item)
}

func TestNormalizeOTPAndOriginAndScrub(t *testing.T) {
	if s, err := NormalizeOTPSecret("otpauth://totp/x?secret=jbsw y3dp&digits=8&period=60&algorithm=sha-256"); err != nil || s != "JBSWY3DP|8|60|SHA256" {
		t.Fatalf("otp %q %v", s, err)
	}
	if s, _ := NormalizeOTPSecret(" jbsw-y3dp== "); s != "JBSWY3DP" {
		t.Fatalf("plain %q", s)
	}
	for _, bad := range []string{"otpauth://hotp/x?secret=A", "otpauth://totp/x?secret=A&digits=x", "otpauth://totp/x?secret=A&digits=9", "189!"} {
		if _, err := NormalizeOTPSecret(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if code, _ := TOTPAt("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", 8, 30, 59); code != "94287082" {
		t.Fatalf("rfc 6238 vector %q", code)
	}
	for in, want := range map[string]string{"https://Example.com:443/x": "https://example.com", "http://h:8080": "http://h:8080"} {
		if got, err := NormalizeOrigin(in); got != want || err != nil {
			t.Fatalf("%s %q %v", in, got, err)
		}
	}
	if _, err := NormalizeOrigin("example.com"); err == nil {
		t.Fatal("schemeless origin")
	}
	got := ScrubSecretFromText("bad pw hunter2 password=hunter2 ok", pyjson.ObjectOf("password", "hunter2"))
	if got != "bad pw [REDACTED] password=[REDACTED] ok" {
		t.Fatalf("scrub %q", got)
	}
}

func TestDeleteKeyRefRemovesTheNamedEntryAndAgreesOnAGoneOne(t *testing.T) {
	k := installMemoryKeyring(t)
	k.entries[[2]string{VaultKeychainService, "acct-1"}] = "k1"
	k.entries[[2]string{VaultKeychainService, "acct-2"}] = "k2"
	ref := map[string]any{"backend": "keychain", "service": VaultKeychainService, "account": "acct-1"}
	if !VaultDeleteKeyRef(ref) || k.count() != 1 {
		t.Fatalf("first delete left %d entries", k.count())
	}
	if !VaultDeleteKeyRef(ref) {
		t.Fatal("an already deleted entry reads as present")
	}
	useKeyring(t, nil, errors.New("no backend"))
	if VaultDeleteKeyRef(map[string]any{"account": "acct-2"}) {
		t.Fatal("an unreachable keychain reported the entry gone")
	}
}
