package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/vault"
)

var failures int

func check(ok bool, format string, args ...any) {
	status := "ok  "
	if !ok {
		status = "FAIL"
		failures++
	}
	fmt.Printf("%s %s\n", status, fmt.Sprintf(format, args...))
}

func must(err error, what string) {
	if err != nil {
		fmt.Printf("FATAL %s: %v\n", what, err)
		os.Exit(2)
	}
}

// independent fernet
func pfKey(text string) ([]byte, []byte) {
	raw, err := base64.URLEncoding.DecodeString(strings.TrimSpace(text))
	must(err, "decode key")
	return raw[:16], raw[16:]
}

func pfEncrypt(keyText string, plain []byte, ts uint64, iv []byte) string {
	sign, enc := pfKey(keyText)
	pad := 16 - len(plain)%16
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(enc)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	msg := []byte{0x80}
	msg = binary.BigEndian.AppendUint64(msg, ts)
	msg = append(append(msg, iv...), ct...)
	m := hmac.New(sha256.New, sign)
	m.Write(msg)
	return base64.URLEncoding.EncodeToString(m.Sum(msg))
}

func pfEncryptRawPad(keyText string, padded []byte, ts uint64, iv []byte) string {
	sign, enc := pfKey(keyText)
	block, _ := aes.NewCipher(enc)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	msg := []byte{0x80}
	msg = binary.BigEndian.AppendUint64(msg, ts)
	msg = append(append(msg, iv...), ct...)
	m := hmac.New(sha256.New, sign)
	m.Write(msg)
	return base64.URLEncoding.EncodeToString(m.Sum(msg))
}

func pfDecrypt(keyText, token string) ([]byte, uint64, []byte, error) {
	sign, enc := pfKey(keyText)
	data, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return nil, 0, nil, err
	}
	if len(data) < 57 || data[0] != 0x80 {
		return nil, 0, nil, errors.New("bad layout")
	}
	m := hmac.New(sha256.New, sign)
	m.Write(data[:len(data)-32])
	if !hmac.Equal(m.Sum(nil), data[len(data)-32:]) {
		return nil, 0, nil, errors.New("bad hmac")
	}
	iv := data[9:25]
	ct := data[25 : len(data)-32]
	block, _ := aes.NewCipher(enc)
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ct)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > 16 {
		return nil, 0, nil, errors.New("bad pad")
	}
	for _, b := range plain[len(plain)-pad:] {
		if int(b) != pad {
			return nil, 0, nil, errors.New("bad pad bytes")
		}
	}
	return plain[:len(plain)-pad], binary.BigEndian.Uint64(data[1:9]), iv, nil
}

// independent HOTP
func hotp(key []byte, counter uint64, digits int, h func() hash.Hash) string {
	m := hmac.New(h, key)
	m.Write(binary.BigEndian.AppendUint64(nil, counter))
	d := m.Sum(nil)
	off := d[len(d)-1] & 0xf
	code := (uint32(d[off])&0x7f)<<24 | uint32(d[off+1])<<16 | uint32(d[off+2])<<8 | uint32(d[off+3])
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, code%mod)
}

func newVault() *vault.Vault {
	resolver := home.New(hostinfo.New())
	return vault.New(resolver, vault.NewKeychain(), func(context.Context, string) string { return "" })
}

func homeDir() string { return os.Getenv("ICEBATH_HOME") }

func vaultPaths(h string) (data, key, ref string) {
	return filepath.Join(h, "vault", "vault.json.enc"), filepath.Join(h, "vault", "vault.key"), filepath.Join(h, "vault", "vault.keyref")
}

func readDoc(h string) (string, error) {
	data, key, _ := vaultPaths(h)
	k, err := os.ReadFile(key)
	if err != nil {
		return "", err
	}
	tok, err := os.ReadFile(data)
	if err != nil {
		return "", err
	}
	plain, _, _, err := pfDecrypt(string(k), string(tok))
	return string(plain), err
}

func childAdd(n int, tag string) {
	ctx := context.Background()
	v := newVault()
	st, err := v.Store(ctx, vault.Options{})
	must(err, "child store")
	for i := 0; i < n; i++ {
		_, err := st.Add(ctx, "login", fmt.Sprintf("%s-%d", tag, i), vault.NewPayload(vault.Field{Name: "identifier_type", Value: "username"},
			vault.Field{Name: "identifier", Value: "u"}, vault.Field{Name: "password", Value: "pw-" + tag}), "https://example.com")
		must(err, "child add")
	}
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == "child-add" {
		var n int
		fmt.Sscan(os.Args[2], &n)
		childAdd(n, os.Args[3])
		return
	}
	root, err := os.MkdirTemp("", "vaultprobe")
	must(err, "tempdir")
	defer os.RemoveAll(root)
	realBus := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
	h := filepath.Join(root, "home")
	os.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()

	fernetVectors()
	totpVectors()
	storeTests(ctx, root, h)
	otpTests(ctx)
	originTests()
	blindTests(ctx)
	scriptTests(ctx)
	classifierTests()
	keyTests(ctx, root)
	lockTests(ctx, root)
	managerTests(ctx, root)
	pluginTests(ctx)
	sessionScopeTests(ctx, root)
	if realBus != "" {
		os.Setenv("DBUS_SESSION_BUS_ADDRESS", realBus)
		secretServiceTest(ctx, root)
		os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
	} else {
		fmt.Println("info no session bus address, Secret Service test not run")
	}
	fmt.Printf("\nfailures: %d\n", failures)
}

func fernetVectors() {
	fmt.Println("== fernet spec vector")
	iv := make([]byte, 16)
	for i := range iv {
		iv[i] = byte(i)
	}
	tok := pfEncrypt("cw_0x689RpI-jtRR7oE8h_eQsKImvJapLeSbXpwF4e4=", []byte("hello"), 499162800, iv)
	want := "gAAAAAAdwJ6wAAECAwQFBgcICQoLDA0ODy021cpGVWKZ_eEwCGM4BLLF_5CV9dOPmrhuVUPgJobwOz7JcbmrR64jVmpU4IwqDA=="
	check(tok == want, "probe fernet reproduces spec token %s", tok)
	plain, ts, _, err := pfDecrypt("cw_0x689RpI-jtRR7oE8h_eQsKImvJapLeSbXpwF4e4=", want)
	check(err == nil && string(plain) == "hello" && ts == 499162800, "probe fernet decrypts spec token %q %d %v", plain, ts, err)
}

func totpVectors() {
	fmt.Println("== RFC 4226 and 6238 vectors against probe HOTP")
	k1 := []byte("12345678901234567890")
	hotps := []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"}
	for i, w := range hotps {
		check(hotp(k1, uint64(i), 6, sha1.New) == w, "rfc4226 counter %d", i)
	}
	k256 := []byte("12345678901234567890123456789012")
	k512 := []byte("1234567890123456789012345678901234567890123456789012345678901234")
	type vec struct {
		t                int64
		s1, s256, s512   string
	}
	for _, v := range []vec{{59, "94287082", "46119246", "90693936"}, {1111111109, "07081804", "68084774", "25091201"},
		{1111111111, "14050471", "67062674", "99943326"}, {1234567890, "89005924", "91819424", "93441116"},
		{2000000000, "69279037", "90698825", "38618901"}, {20000000000, "65353130", "77737706", "47863826"}} {
		c := uint64(v.t / 30)
		check(hotp(k1, c, 8, sha1.New) == v.s1 && hotp(k256, c, 8, sha256.New) == v.s256 && hotp(k512, c, 8, sha512.New) == v.s512,
			"rfc6238 t=%d", v.t)
	}
}

func loginPayload(id, pw string, extra ...vault.Field) vault.Payload {
	return vault.NewPayload(append([]vault.Field{{Name: "identifier_type", Value: "email"}, {Name: "identifier", Value: id},
		{Name: "password", Value: pw}}, extra...)...)
}

func storeTests(ctx context.Context, root, h string) {
	fmt.Println("== store")
	v := newVault()
	st, err := v.Store(ctx, vault.Options{})
	must(err, "store")
	m, err := st.Add(ctx, "login", "  GitHub  ", loginPayload(" me@example.com ", "hunter2"), "HTTPS://GitHub.com:443/login")
	must(err, "add")
	check(m.Label == "GitHub" && m.Origin == "https://github.com" && m.Identifier == "me@example.com" && m.IdentifierType == "email" &&
		strings.HasPrefix(m.ID, "vault_") && len(m.ID) == 18, "add meta %+v", m)
	check(strings.HasSuffix(m.CreatedAt, "+00:00"), "created_at %s", m.CreatedAt)
	data, keyPath, _ := vaultPaths(h)
	for _, p := range []string{filepath.Join(h, "vault"), data, keyPath} {
		fi, err := os.Stat(p)
		must(err, "stat")
		want := fs.FileMode(0o600)
		if fi.IsDir() {
			want = 0o700
		}
		check(fi.Mode().Perm() == want, "mode of %s is %v", filepath.Base(p), fi.Mode().Perm())
	}
	keyText, _ := os.ReadFile(keyPath)
	raw, kerr := base64.URLEncoding.DecodeString(string(keyText))
	check(kerr == nil && len(raw) == 32 && len(keyText) == 44, "key file is 44 url-safe base64 chars without newline %q", keyText)
	tok, _ := os.ReadFile(data)
	plain, ts, iv1, err := pfDecrypt(string(keyText), string(tok))
	check(err == nil, "probe decrypts vault file %v", err)
	now := time.Now().Unix()
	check(int64(ts) <= now && now-int64(ts) < 30, "token timestamp %d near now %d", ts, now)
	wantDoc := fmt.Sprintf(`{"version": 1, "items": [{"id": "%s", "kind": "login", "label": "GitHub", "origin": "https://github.com", "created_at": "%s", "identifier_type": "email", "identifier": "me@example.com", "secret": {"password": "hunter2"}}]}`, m.ID, m.CreatedAt)
	check(string(plain) == wantDoc, "document bytes\n  got  %s\n  want %s", plain, wantDoc)
	m2, err := st.Add(ctx, "payment", "Card", vault.NewPayload(vault.Field{Name: "card_number", Value: "4111 1111"},
		vault.Field{Name: "exp_month", Value: "7"}, vault.Field{Name: "exp_year", Value: "2031"}, vault.Field{Name: "cvc", Value: "123"},
		vault.Field{Name: "nickname", Value: "dropped"}, vault.Field{Name: "cardholder_name", Value: "  "}), "")
	must(err, "add payment")
	check(m2.Origin == "" && m2.Identifier == "", "payment meta %+v", m2)
	tok2, _ := os.ReadFile(data)
	_, _, iv2, _ := pfDecrypt(string(keyText), string(tok2))
	check(!bytes.Equal(iv1, iv2), "fresh IV per write")
	doc, _ := readDoc(h)
	check(strings.Contains(doc, `"secret": {"card_number": "4111 1111", "exp_month": "7", "exp_year": "2031", "cvc": "123"}`) &&
		strings.Contains(doc, `"origin": null`) && strings.Contains(doc, `"identifier_type": null, "identifier": null`), "payment record %s", doc)
	_, err = st.Add(ctx, "payment", "Bad", vault.NewPayload(vault.Field{Name: "card_number", Value: "4"}), "")
	check(err != nil && err.Error() == "payment items require exp_month, exp_year, cvc" && errors.Is(err, vault.ErrInvalid), "missing fields %v", err)
	_, err = st.Add(ctx, "note", "x", vault.NewPayload(), "")
	check(err != nil && err.Error() == "unknown vault kind 'note' (expected one of ('login', 'payment', 'address'))", "kind %v", err)
	_, err = st.Add(ctx, "login", "x", vault.NewPayload(vault.Field{Name: "identifier_type", Value: "nick"}), "https://a.com")
	check(err != nil && err.Error() == "identifier_type must be one of ('email', 'phone', 'username')", "id type %v", err)
	_, err = st.Add(ctx, "login", "x", loginPayload("", "pw"), "https://a.com")
	check(err != nil && err.Error() == "login items require identifier and password", "identifier %v", err)
	_, err = st.Add(ctx, "login", "x", loginPayload("a", "pw"), "")
	check(err != nil && err.Error() == "origin is required for login items", "origin %v", err)
	_, err = st.Add(ctx, "login", " ", loginPayload("a", "pw"), "https://a.com")
	check(err != nil && err.Error() == "label is required", "label %v", err)

	p, err := vault.ParsePayload([]byte(`{"identifier_type":"username","identifier":7,"password":12345,"otp_secret":null,"extra":{"a":1}}`))
	must(err, "parse payload")
	m3, err := st.Add(ctx, "login", "Num", p, "https://n.example")
	must(err, "add numeric")
	check(m3.Identifier == "7", "numeric identifier as text %q", m3.Identifier)
	doc, _ = readDoc(h)
	check(strings.Contains(doc, `"identifier": "7", "secret": {"password": 12345}`), "raw password kept %s", doc[strings.Index(doc, `"label": "Num"`):])
	b, ok, err := v.Backend(ctx, vault.Options{}, m3.ID)
	must(err, "backend")
	sec, err := b.ResolvePassword(ctx, m3.ID)
	check(ok && err == nil && sec.Reveal(vault.PasswordField) == "12345", "numeric password resolves as text")
	_, err = vault.ParsePayload([]byte(`[1]`))
	check(errors.Is(err, vault.ErrInvalid), "payload must be object %v", err)

	list := listAll(ctx, v)
	check(len(list) == 3, "list has 3 items")
	removed, err := st.Remove(ctx, m2.ID)
	check(removed && err == nil, "remove existing")
	removed, err = st.Remove(ctx, "vault_nope")
	check(!removed && err == nil, "remove missing")
	check(len(listAll(ctx, v)) == 2, "list after remove")

	fmt.Println("== store crafted documents")
	crafted := `{"version": 1, "items": [{"id": "vault_aaaaaaaaaaaa", "kind": "login", "label": "Sur\ud800rogate é", "origin": "https://x.example", "created_at": "c", "identifier_type": "email", "identifier": "i", "secret": {"password": "p😀", "otp_secret": ""}, "future": {"b": [1, 2.5, true, null]}}], "extra": 1}`
	writeToken(h, crafted)
	ms := listAll(ctx, v)
	check(len(ms) == 1 && ms[0].Label == "Sur�rogate é" && !ms[0].HasOTP, "crafted list %+v", ms)
	mm, err := st.Add(ctx, "address", "Home", vault.NewPayload(vault.Field{Name: "address_line1", Value: "1 Main"},
		vault.Field{Name: "city", Value: "Town"}, vault.Field{Name: "postal_code", Value: "1"}, vault.Field{Name: "country", Value: "Ūland"}), "https://shop.example:8443")
	must(err, "add after crafted")
	doc, _ = readDoc(h)
	wantPrefix := `{"version": 1, "items": [{"id": "vault_aaaaaaaaaaaa", "kind": "login", "label": "Sur\ud800rogate é", "origin": "https://x.example", "created_at": "c", "identifier_type": "email", "identifier": "i", "secret": {"password": "p😀", "otp_secret": ""}, "future": {"b": [1, 2.5, true, null]}}, {"id": "` + mm.ID
	check(strings.HasPrefix(doc, wantPrefix), "crafted item round trips byte for byte, top level extra dropped\n  got %s", doc)
	check(strings.Contains(doc, `"country": "Ūland"`) && strings.Contains(doc, `"origin": "https://shop.example:8443"`), "address ascii escapes")
	dup := `{"version": 1, "items": [{"id": "vault_first", "id": "vault_dupe", "kind": "login", "label": "L", "secret": {"password": "a", "password": "b"}}], "items": [{"id": "vault_last", "kind": "login", "label": "Last", "secret": {}}]}`
	writeToken(h, dup)
	ms = listAll(ctx, v)
	check(len(ms) == 1 && ms[0].ID == "vault_last", "repeated names keep the last value %+v", ms)

	fmt.Println("== corrupt and foreign files")
	writeToken(h, `{"version": 2, "items": [{"id": "vault_v2", "kind": "login", "label": "V2"}]}`)
	before, _ := os.ReadFile(data)
	ms = listAll(ctx, v)
	check(len(ms) == 1 && ms[0].ID == "vault_v2", "newer version still lists")
	_, err = st.Add(ctx, "login", "x", loginPayload("a", "b"), "https://a.com")
	after, _ := os.ReadFile(data)
	check(errors.Is(err, vault.ErrNewerFormat) && bytes.Equal(before, after), "newer version refuses writes and leaves file %v", err)
	_, err = st.Remove(ctx, "vault_v2")
	after, _ = os.ReadFile(data)
	check(errors.Is(err, vault.ErrNewerFormat) && bytes.Equal(before, after), "newer version refuses remove %v", err)

	writeToken(h, `{"version": 1, "items": []}`)
	good, _ := os.ReadFile(data)
	flipped := bytes.Clone(good)
	flipped[len(flipped)/2] ^= 0x01
	if flipped[len(flipped)/2] == '=' {
		flipped[len(flipped)/2+1] ^= 1
	}
	expectBroken(ctx, v, st, data, flipped, vault.ErrUndecryptable, "flipped byte")
	expectBroken(ctx, v, st, data, good[:len(good)/2], vault.ErrUndecryptable, "truncated")
	expectBroken(ctx, v, st, data, []byte("garbage!"), vault.ErrUndecryptable, "garbage")
	key := string(mustRead(keyPath))
	ivz := make([]byte, 16)
	expectBroken(ctx, v, st, data, []byte(pfEncrypt(key, []byte("not json"), uint64(time.Now().Unix()), ivz)), vault.ErrCorrupt, "not json")
	expectBroken(ctx, v, st, data, []byte(pfEncrypt(key, []byte(`[1,2]`), uint64(time.Now().Unix()), ivz)), vault.ErrCorrupt, "array document")
	expectBroken(ctx, v, st, data, []byte(pfEncrypt(key, []byte("{\"items\": [\"x\"]}"), uint64(time.Now().Unix()), ivz)), vault.ErrCorrupt, "non-object record")
	expectBroken(ctx, v, st, data, []byte(pfEncrypt(key, []byte{0xff, 0xfe}, uint64(time.Now().Unix()), ivz)), vault.ErrCorrupt, "invalid utf8")
	expectBroken(ctx, v, st, data, []byte(pfEncrypt(key, []byte("\xef\xbb\xbf{}"), uint64(time.Now().Unix()), ivz)), vault.ErrCorrupt, "bom")
	expectBroken(ctx, v, st, data, []byte(pfEncrypt(key, []byte(`{"items": [NaN]}`), uint64(time.Now().Unix()), ivz)), vault.ErrCorrupt, "nan literal")
	badPad := append([]byte(`{"items": []}`), 0, 0, 3)
	expectBroken(ctx, v, st, data, []byte(pfEncryptRawPad(key, badPad, uint64(time.Now().Unix()), ivz)), vault.ErrUndecryptable, "bad padding under valid hmac")
	zeroPad := append([]byte(`{"items": [] }`), 0, 0)
	expectBroken(ctx, v, st, data, []byte(pfEncryptRawPad(key, zeroPad, uint64(time.Now().Unix()), ivz)), vault.ErrUndecryptable, "zero pad byte")
	other := make([]byte, 32)
	rand.Read(other)
	otherKey := base64.URLEncoding.EncodeToString(other)
	expectBroken(ctx, v, st, data, []byte(pfEncrypt(otherKey, []byte(`{"items": []}`), uint64(time.Now().Unix()), ivz)), vault.ErrUndecryptable, "other key")
	stdKey := base64.StdEncoding.EncodeToString(other)
	os.WriteFile(keyPath, []byte("\n  "+stdKey+" \n"), 0o600)
	os.WriteFile(data, []byte(pfEncrypt(otherKey, []byte(`{"items": [{"id":"vault_std","kind":"login","label":"S"}]}`), 1, ivz)), 0o600)
	ms = listAll(ctx, v)
	check(len(ms) == 1 && ms[0].ID == "vault_std", "standard alphabet key with whitespace opens the vault")
	os.WriteFile(keyPath, []byte("short"), 0o600)
	_, err = st.Remove(ctx, "vault_std")
	check(errors.Is(err, vault.ErrBadKey), "malformed key %v", err)
	os.Remove(keyPath)
	before, _ = os.ReadFile(data)
	_, err = st.Add(ctx, "login", "x", loginPayload("a", "b"), "https://a.com")
	after, _ = os.ReadFile(data)
	_, kerr = os.Stat(keyPath)
	check(errors.Is(err, vault.ErrKeyMissing) && bytes.Equal(before, after) && errors.Is(kerr, fs.ErrNotExist), "missing key never mints over ciphertext %v", err)
	os.WriteFile(keyPath, []byte(otherKey), 0o644)
	ms = listAll(ctx, v)
	fi, _ := os.Stat(keyPath)
	check(len(ms) == 1 && fi.Mode().Perm() == 0o600, "key file tightened to 0600 on read %v", fi.Mode().Perm())
	os.WriteFile(data, nil, 0o600)
	check(len(listAll(ctx, v)) == 0, "empty file lists nothing")
}

func mustRead(p string) []byte {
	b, err := os.ReadFile(p)
	must(err, "read "+p)
	return b
}

func writeToken(h, doc string) {
	data, keyPath, _ := vaultPaths(h)
	key := string(mustRead(keyPath))
	iv := make([]byte, 16)
	rand.Read(iv)
	must(os.WriteFile(data, []byte(pfEncrypt(key, []byte(doc), uint64(time.Now().Unix()), iv)), 0o600), "write token")
}

func expectBroken(ctx context.Context, v *vault.Vault, st *vault.Store, data string, content []byte, kind error, what string) {
	must(os.WriteFile(data, content, 0o600), "write broken")
	_, lerr := listErr(ctx, v)
	_, aerr := st.Add(ctx, "login", "x", loginPayload("a", "b"), "https://a.com")
	after, _ := os.ReadFile(data)
	check(errors.Is(lerr, kind) && errors.Is(aerr, kind) && bytes.Equal(after, content), "%s gives %v and leaves the file", what, lerr)
}

func listErr(ctx context.Context, v *vault.Vault) ([]vault.ItemMeta, error) {
	b, _, err := v.Backend(ctx, vault.Options{}, "vault_x")
	if err != nil {
		return nil, err
	}
	return b.List(ctx)
}

func listAll(ctx context.Context, v *vault.Vault) []vault.ItemMeta {
	ms, err := listErr(ctx, v)
	must(err, "list")
	return ms
}

func otpTests(ctx context.Context) {
	fmt.Println("== totp through the local backend")
	v := newVault()
	st, err := v.Store(ctx, vault.Options{})
	must(err, "store")
	seed := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	key, _ := base32.StdEncoding.DecodeString(seed)
	cases := []struct {
		input, stored string
		digits        int
		period        int64
		hash          func() hash.Hash
	}{
		{"gezd gnbv-gy3t qojq gezd gnbv gy3t qojq====", seed, 6, 30, sha1.New},
		{"otpauth://totp/Ex:me?secret=" + seed + "&issuer=Ex", seed, 6, 30, sha1.New},
		{"OTPAUTH://TOTP/x?SECRET=" + strings.ToLower(seed) + "&DIGITS=8&Period=60&Algorithm=sha-256", seed + "|8|60|SHA256", 8, 60, sha256.New},
		{"otpauth://totp/x?secret=" + seed + "&digits=7&algorithm=SHA512", seed + "|7|30|SHA512", 7, 30, sha512.New},
		{"otpauth://totp/x?secret=" + seed + "&digits=+08&period=1_0", seed + "|8|10|SHA1", 8, 10, sha1.New},
	}
	for i, c := range cases {
		m, err := st.Add(ctx, "login", fmt.Sprintf("otp%d", i), loginPayload("u", "p", vault.Field{Name: "otp_secret", Value: c.input}), "https://otp.example")
		if err != nil {
			check(false, "add otp %q: %v", c.input, err)
			continue
		}
		doc, _ := readDoc(homeDir())
		check(strings.Contains(doc, `"otp_secret": "`+c.stored+`"`) && m.HasOTP, "stored form %s", c.stored)
		b, _, _ := v.Backend(ctx, vault.Options{}, m.ID)
		for attempt := 0; attempt < 3; attempt++ {
			before := time.Now().UnixNano()
			code, err := b.ResolveOTP(ctx, m.ID)
			afterT := time.Now().UnixNano()
			step := c.period * int64(time.Second)
			if before/step != afterT/step {
				continue
			}
			want := hotp(key, uint64(before/step), c.digits, c.hash)
			check(err == nil && code.Reveal(vault.CodeField) == want, "code %q matches independent %s", code.Reveal(vault.CodeField), want)
			break
		}
	}
	for _, bad := range []struct{ in, msg string }{
		{"otpauth://hotp/x?secret=" + seed, "only otpauth://totp links are supported (counter-based HOTP is not)"},
		{"otpauth://totp/x?secret=" + seed + "&digits=9", "unsupported otpauth:// parameters (digits 6-8, period > 0, SHA1/SHA256/SHA512)"},
		{"otpauth://totp/x?secret=" + seed + "&period=0", "unsupported otpauth:// parameters (digits 6-8, period > 0, SHA1/SHA256/SHA512)"},
		{"otpauth://totp/x?secret=" + seed + "&algorithm=MD5", "unsupported otpauth:// parameters (digits 6-8, period > 0, SHA1/SHA256/SHA512)"},
		{"otpauth://totp/x?secret=" + seed + "&digits=6.0", "otpauth:// digits/period must be integers"},
		{"otpauth://totp/x?issuer=a", "authenticator key must be a base32 secret or an otpauth:// URI"},
		{"ABC1", "authenticator key must be a base32 secret or an otpauth:// URI"},
	} {
		_, err := st.Add(ctx, "login", "bad", loginPayload("u", "p", vault.Field{Name: "otp_secret", Value: bad.in}), "https://otp.example")
		check(err != nil && err.Error() == bad.msg && errors.Is(err, vault.ErrInvalid), "otp %q refused with %v", bad.in, err)
	}
	m, _ := st.Add(ctx, "login", "plain", loginPayload("u", "p"), "https://otp.example")
	b, _, _ := v.Backend(ctx, vault.Options{}, m.ID)
	code, err := b.ResolveOTP(ctx, m.ID)
	check(err == nil && len(code.Fields()) == 0, "login without seed has no code")
}

func originTests() {
	fmt.Println("== origins")
	ok := map[string]string{
		"https://Example.COM":                      "https://example.com",
		"https://example.com:443/path?q#f":         "https://example.com",
		"http://example.com:80":                    "http://example.com",
		"http://example.com:443":                   "http://example.com:443",
		"https://example.com:0443":                 "https://example.com",
		"https://example.com:8443":                 "https://example.com:8443",
		"https://user:pass@example.com:444/x":      "https://example.com:444",
		"https://example.com@evil.com":             "https://evil.com",
		"https://EXAMPLE.com:443@evil.com":         "https://evil.com",
		"https://example.com\\@evil.com":           "https://evil.com",
		"https://example.com.":                     "https://example.com.",
		"https://[::1]:8080":                       "https://::1:8080",
		"https://[FE80::1%25ETH0]":                 "https://fe80::1%25eth0",
		"https://[v1.x]":                           "https://v1.x",
		"javascript://example.com/%0aalert(1)":     "javascript://example.com",
		"https://example.com:":                     "https://example.com",
		"HTTPS://EXAMPLE.COM:443":                  "https://example.com",
		"  https://example.com　":              "https://example.com",
		"\x00https://example.com":                  "https://example.com",
		"https://exa\tmple.com":                    "https://example.com",
		"https://bücher.de":                        "https://bücher.de",
		"https://İstanbul.com":                     "https://i̇stanbul.com",
		"ftp://example.com:21":                     "ftp://example.com:21",
		"https://@example.com":                     "https://example.com",
		"http://127.0.0.1:8080":                    "http://127.0.0.1:8080",
		"https://example.com#@evil.com":            "https://example.com",
		"https://example.com:0":                    "https://example.com:0",
		"chrome-extension://abcdef/popup.html":     "chrome-extension://abcdef",
		"https://[::ffff:1.2.3.4]":                 "https://::ffff:1.2.3.4",
	}
	for in, want := range ok {
		got, err := vault.NormalizeOrigin(in)
		check(err == nil && got == want, "origin %q -> %q (%v) want %q", in, got, err, want)
	}
	bad := map[string]string{
		"":                                 "origin is required",
		"   ":                              "origin is required",
		"javascript:alert(1)":              "origin must include a scheme (got 'javascript:alert(1)')",
		"data:text/html,hi":                "origin must include a scheme (got 'data:text/html,hi')",
		"example.com":                      "origin must include a scheme (got 'example.com')",
		"//example.com":                    "origin must include a scheme (got '//example.com')",
		"https:///example.com":             "could not parse origin from 'https:///example.com'",
		"1https://x":                       "could not parse origin from '1https://x'",
		"https://user@":                    "could not parse origin from 'https://user@'",
		"https://example.com:99999":        "invalid port in origin 'https://example.com:99999'",
		"https://example.com:-1":           "invalid port in origin 'https://example.com:-1'",
		"https://example.com:80:80":        "invalid port in origin 'https://example.com:80:80'",
		"https://example.com:٨٠":           "invalid port in origin 'https://example.com:٨٠'",
		"https://[1.2.3.4]":                "An IPv4 address cannot be in brackets",
		"https://[vz.x]":                   "IPvFuture address is invalid",
		"https://a[b]@example.com":         "'example.com' does not appear to be an IPv4 or IPv6 address",
		"https://ex[ample.com":             "Invalid IPv6 URL",
		"https://x[::1]":                   "Invalid IPv6 URL",
		"https://[::1]x:80":                "Invalid IPv6 URL",
		"https://[fe80::1%a%b]":            "'fe80::1%a%b' does not appear to be an IPv4 or IPv6 address",
		"https://evil.c℀om":           "netloc 'evil.c℀om' contains invalid characters under NFKC normalization",
		"https://[gg::1]":                  "'gg::1' does not appear to be an IPv4 or IPv6 address",
	}
	for in, want := range bad {
		got, err := vault.NormalizeOrigin(in)
		check(err != nil && err.Error() == want && errors.Is(err, vault.ErrInvalid), "origin %q refused %q (got %q)", in, want, errText(err)+got)
	}
	m := vault.ItemMeta{Origin: "https://a.com", AllowedOrigins: []string{"https://a.com", "https://b.com"}}
	check(m.Binds("https://b.com") && !m.Binds("https://c.a.com") && !m.Binds("https://a.com:443"), "binding is exact")
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

type holder struct {
	S vault.Secret
	s vault.Secret
	F []vault.Fill
	f []vault.Fill
	P vault.Payload
	p vault.Payload
	L vault.Login
	I vault.ForeignItem
	D vault.Field
}

func blindTests(ctx context.Context) {
	fmt.Println("== model blind formatting")
	v := newVault()
	st, _ := v.Store(ctx, vault.Options{})
	const pw = "Sup3rSecretPw!"
	m, err := st.Add(ctx, "login", "blind", loginPayload("blind@example.com", pw), "https://blind.example")
	must(err, "add blind")
	b, _, _ := v.Backend(ctx, vault.Options{}, m.ID)
	sec, _ := b.ResolvePassword(ctx, m.ID)
	controls, _, _ := vault.ParseControls([]byte(`[{"index":2,"type":"password","name":"pw","label":"Password","autocomplete":"current-password","formIndex":0}]`))
	var classified []vault.Classified
	for _, c := range controls {
		if cl, ok := vault.ClassifyLogin(c); ok {
			classified = append(classified, cl)
		}
	}
	fills := vault.SelectPasswordFill(classified, sec)
	payload := loginPayload("blind@example.com", pw)
	hd := holder{S: sec, s: sec, F: fills, f: fills, P: payload, p: payload, L: vault.Login{Identifier: "x", Password: pw},
		I: vault.ForeignItem{Account: "a", Secret: pw}, D: vault.Field{Name: "password", Value: pw}}
	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		outs = append(outs, fmt.Sprintf(verb, hd), fmt.Sprintf(verb, &hd), fmt.Sprintf(verb, sec), fmt.Sprintf(verb, fills), fmt.Sprintf(verb, []any{sec, payload}))
	}
	j, _ := json.Marshal(hd)
	outs = append(outs, string(j))
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "h", hd, "s", sec, "fills", fills, "p", payload, "l", hd.L, "i", hd.I, "d", hd.D)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "h", hd, "s", sec, "fills", fills, "p", payload, "l", hd.L, "i", hd.I, "d", hd.D)
	outs = append(outs, buf.String(), sec.String(), fmt.Sprint(errors.New(sec.Scrub("password: "+pw+" and "+pw))))
	leaked := false
	for _, o := range outs {
		if strings.Contains(o, pw) || strings.Contains(o, hex.EncodeToString([]byte(pw))) || strings.Contains(strings.ToLower(o), strings.ToLower(hex.EncodeToString([]byte(pw)))) {
			leaked = true
			fmt.Println("   leak:", o)
		}
	}
	check(!leaked, "no formatting, json or slog path prints the secret")
	fmt.Println("   sample %+v:", fmt.Sprintf("%+v", hd))
	check(sec.Reveal(vault.PasswordField) == pw, "Reveal is the release")
	check(sec.Scrub("err password="+pw+" end") == "err password=[REDACTED] end", "scrub %q", sec.Scrub("err password="+pw+" end"))
	check(payload.Scrub("id blind@example.com pw "+pw) == "id [REDACTED] pw [REDACTED]", "payload scrub")
	check(vault.Secret{}.Reveal("password") == "" && len(vault.Secret{}.Fields()) == 0, "zero secret is empty")
}

func scriptTests(ctx context.Context) {
	fmt.Println("== page scripts")
	v := newVault()
	st, _ := v.Store(ctx, vault.Options{})
	pw := "p\"a\\ss__NONCE__</script> é😀"
	m, err := st.Add(ctx, "login", "script", loginPayload("s@example.com", pw), "https://script.example")
	must(err, "add script")
	b, _, _ := v.Backend(ctx, vault.Options{}, m.ID)
	sec, _ := b.ResolvePassword(ctx, m.ID)
	cls, _ := vault.ClassifyLogin(vault.Control{Index: 3, Type: "password"})
	fills := vault.SelectPasswordFill([]vault.Classified{cls}, sec)
	script, err := vault.FillScript(fills, "https://script.example", "abcd1234")
	must(err, "fill script")
	wantFills := `const fills = [{"index": 3, "token": "current-password", "value": "p\"a\\ss__NONCE__</script> é😀"}];`
	check(strings.Contains(script, wantFills), "fills JSON exactly as the reference writes it and the value's placeholder text untouched")
	check(strings.Contains(script, `const expectedOrigin = "https://script.example";`) && strings.Contains(script, `const nonce = "abcd1234";`) &&
		strings.Count(script, "__NONCE__") == 1 && !strings.Contains(script, "__FILLS__") && !strings.Contains(script, "__EXPECTED_ORIGIN__"),
		"every placeholder substituted once")
	insp, err := vault.InspectionScript("ab\"cd")
	check(err == nil && strings.Contains(insp, `const nonce = "ab\"cd";`) && strings.Contains(insp, "data-icebath-vault-slot") && !strings.Contains(insp, "hermes"), "inspection script")
	ofills := vault.OTPFills([]vault.Classified{{Control: vault.Control{Index: 4, MaxLength: 1, InForm: true}, Score: 70, Token: "one-time-code"},
		{Control: vault.Control{Index: 5, MaxLength: 1, InForm: true}, Score: 70, Token: "one-time-code"}}, "12")
	os, _ := vault.FillScript(ofills, "https://a.b", "n")
	check(strings.Contains(os, `[{"index": 4, "token": "one-time-code", "value": "1"}, {"index": 5, "token": "one-time-code", "value": "2"}]`), "split otp boxes")
	empty, _ := vault.FillScript(nil, "https://a.b", "n")
	check(strings.Contains(empty, "const fills = [];"), "empty fills list")
}

func classifierTests() {
	fmt.Println("== classifier")
	raw := `[{"autocomplete":"username","formIndex":0,"index":0,"maxLength":null,"label":"","name":"user user","type":"text"},
{"autocomplete":"","formIndex":0,"index":1,"maxLength":null,"label":"Confirm password","name":"pw2","type":"password"},
{"autocomplete":"","formIndex":0,"index":2,"maxLength":null,"label":"Password","name":"pw","type":"password"},
{"autocomplete":"one-time-code","formIndex":1,"index":3,"maxLength":6,"label":"","name":"","type":"text"},
{"autocomplete":"","form_index":null,"index":"4","max_length":1,"label":"Verification code digit","name":"","type":"tel"},
{"autocomplete":"cc-number","formIndex":2,"index":5,"maxLength":19,"label":"","name":"","type":"text"},
{"autocomplete":"","formIndex":2,"index":6,"maxLength":null,"label":"Expiry date (MM/YY)","name":"exp","type":"text"},
{"autocomplete":"country","formIndex":2,"index":7,"maxLength":null,"label":"","name":"","type":"select"},
"skip", 5]`
	controls, ok, err := vault.ParseControls([]byte(raw))
	check(ok && err == nil && len(controls) == 8, "parsed %d controls", len(controls))
	if len(controls) < 8 {
		return
	}
	check(controls[4].Index == 4 && !controls[4].InForm && controls[4].MaxLength == 1, "snake case and string index %+v", controls[4])
	var login []vault.Classified
	for _, c := range controls {
		if cl, ok := vault.ClassifyLogin(c); ok {
			login = append(login, cl)
		}
	}
	check(len(login) == 2 && login[0].Token == "username" && login[0].Score == 100 && login[1].Control.Index == 2 && login[1].Score == 90,
		"login classes %+v", login)
	otp := vault.ClassifyOTP(controls)
	check(len(otp) == 2 && otp[0].Score == 100 && otp[1].Score == 70, "otp classes %d", len(otp))
	var checkout []vault.Classified
	for _, c := range controls {
		if cl, ok := vault.ClassifyCheckout(c); ok {
			checkout = append(checkout, cl)
		}
	}
	tokens := []string{}
	for _, c := range checkout {
		tokens = append(tokens, c.Token)
	}
	check(strings.Join(tokens, ",") == "cc-number,cc-exp,country-name", "checkout tokens %v", tokens)
	_, ok, _ = vault.ParseControls([]byte(`{"a":1}`))
	check(!ok, "non-list inspection output")
	_, _, err = vault.ParseControls([]byte(`[{"index":"x"}]`))
	check(err != nil, "non-integer index refused")
}

func keyTests(ctx context.Context, root string) {
	fmt.Println("== key storage without a keychain")
	h := filepath.Join(root, "keyhome")
	hctx := home.WithOverride(ctx, h)
	v := newVault()
	st, err := v.Store(hctx, vault.Options{KeyStorage: " KeyChain "})
	must(err, "store")
	_, err = st.Add(hctx, "login", "k", loginPayload("a", "b"), "https://k.example")
	must(err, "add keychain mode without keychain")
	data, keyPath, refPath := vaultPaths(h)
	_, kerr := os.Stat(keyPath)
	_, rerr := os.Stat(refPath)
	check(kerr == nil && errors.Is(rerr, fs.ErrNotExist), "keychain mode falls back to the 0600 key file when no keychain is reachable")
	loc, err := st.KeyLocation()
	check(err == nil && loc.Storage == "file" && loc.Path == keyPath, "location %+v", loc)
	_, err = st.MigrateKey(hctx, "keychain")
	check(errors.Is(err, vault.ErrKeyStorage) && strings.Contains(err.Error(), "could not move the vault key to the OS keychain") &&
		strings.Contains(err.Error(), "it stays in "+keyPath), "migrate to keychain refused %v", err)
	_, err = os.Stat(keyPath)
	check(err == nil, "key file kept after refused migration")
	_, err = st.MigrateKey(hctx, "vault")
	check(err != nil && err.Error() == "key storage must be 'file' or 'keychain'", "bad target %v", err)
	out, err := st.MigrateKey(hctx, "file")
	check(out == "file" && err == nil, "migrate to file is a no-op on a file vault")
	ref := `{"storage": "keychain", "service": "icebath vault key", "account": "vault-probe", "vault_dir": "` + filepath.Join(h, "vault") + `"}`
	os.WriteFile(refPath, []byte("\xef\xbb\xbf"+ref), 0o600)
	keyText := mustRead(keyPath)
	os.Remove(keyPath)
	before := mustRead(data)
	_, err = listErrCtx(hctx, v)
	check(errors.Is(err, vault.ErrKeyMissing) && strings.Contains(err.Error(), "the OS keychain is not reachable here") &&
		strings.Contains(err.Error(), "vault.json.enc in "+filepath.Join(h, "vault")+" was left untouched"), "unreachable keychain with ciphertext %v", err)
	check(bytes.Equal(before, mustRead(data)), "ciphertext untouched")
	loc, _ = st.KeyLocation()
	check(loc.Storage == "keychain" && loc.Account == "vault-probe" && loc.Service == "icebath vault key", "keychain location %+v", loc)
	os.WriteFile(keyPath, keyText, 0o600)
	loc, _ = st.KeyLocation()
	check(loc.Storage == "file" && loc.LeftAccount == "vault-probe", "file location with a leftover entry %+v", loc)
	os.WriteFile(refPath, []byte(`{"storage": "file"}`), 0o600)
	_, err = listErrCtx(hctx, v)
	check(err == nil, "a key file wins over any pointer")
	os.Remove(keyPath)
	_, err = listErrCtx(hctx, v)
	check(errors.Is(err, vault.ErrKeyRef) && err.Error() == "vault.keyref does not describe a keychain entry; see `icebath vault key`", "bad pointer %v", err)
	os.WriteFile(refPath, []byte(`{nope`), 0o600)
	_, err = listErrCtx(hctx, v)
	check(errors.Is(err, vault.ErrKeyRef) && strings.Contains(err.Error(), "is unreadable (invalid JSON)"), "unreadable pointer %v", err)
	os.Remove(refPath)
	os.Remove(data)
	h2 := filepath.Join(root, "fresh")
	st2, _ := v.Store(home.WithOverride(ctx, h2), vault.Options{})
	_, err = st2.MigrateKey(home.WithOverride(ctx, h2), "keychain")
	check(errors.Is(err, vault.ErrKeyStorage) && strings.HasPrefix(err.Error(), "cannot use the OS keychain: "), "no keychain for a fresh vault %v", err)
	loc, _ = st2.KeyLocation()
	check(loc.Storage == "none", "fresh location %+v", loc)
}

func listErrCtx(ctx context.Context, v *vault.Vault) ([]vault.ItemMeta, error) { return listErr(ctx, v) }

func lockTests(ctx context.Context, root string) {
	fmt.Println("== cross process lock")
	h := filepath.Join(root, "lockhome")
	exe, err := os.Executable()
	must(err, "exe")
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(exe, "child-add", "8", fmt.Sprintf("c%d", i))
			cmd.Env = append(os.Environ(), "ICEBATH_HOME="+h)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- fmt.Errorf("%v %s", err, out)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		check(false, "child failed %v", e)
	}
	hctx := home.WithOverride(ctx, h)
	ms, err := listErrCtx(hctx, newVault())
	check(err == nil && len(ms) == 48, "six processes adding eight items each leave %d items", len(ms))
	_, keyPath, _ := vaultPaths(h)
	check(len(mustRead(keyPath)) == 44, "one key survived concurrent first writes")
}

func writeScript(path, body string) {
	must(os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755), "write script")
}

func managerTests(ctx context.Context, root string) {
	fmt.Println("== password manager backends against fake CLIs")
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	oplog := filepath.Join(root, "op.log")
	opExpired := filepath.Join(root, "op.expired")
	opSlow := filepath.Join(root, "op.slow")
	writeScript(filepath.Join(bin, "op"), `LOG='`+oplog+`'
{ printf 'ARGS:'; for a in "$@"; do printf ' [%s]' "$a"; done; echo; env | sort | sed 's/^/ENV /'; } >> "$LOG"
if [ -e '`+opExpired+`' ]; then echo "[ERROR] 2024/01/01 session expired, sign in to create a new session" >&2; exit 1; fi
case "$1 $2" in
"signin --raw")
  IFS= read -r pw; printf 'STDIN [%s]\n' "$pw" >> "$LOG"
  if [ -e '`+opSlow+`' ]; then sleep 2; fi
  if [ "$pw" = "master" ]; then echo " tok-123 "; else printf '\033[31m[ERROR]\033[0m invalid master password\n' >&2; exit 1; fi ;;
"item list")
  cat <<'EOF'
[{"id":"abc","title":"Example","vault":{"id":"v1"},"urls":[{"href":"https://Example.com/login"},{"label":"x"},{"href":"androidapp://com.example"},{"href":"https://example.org:443"},{"href":"https://Example.com/other"}],"additional_information":" alice@example.com ","created_at":"2024-01-01T00:00:00Z"},
 {"id":"appOnly","title":"","vault":{"id":"v2"},"urls":[{"href":"androidapp://x"}]},
 {"id":"nourl","title":"x","vault":null},
 {"id":"strurls","title":"y","urls":"https://ignored.example"}]
EOF
  ;;
"item get")
  case "$*" in *--otp*) echo " 123456 " ;; *) printf 'pa ss\r\n' ;; esac ;;
esac
`)
	bwlog := filepath.Join(root, "bw.log")
	writeScript(filepath.Join(bin, "bw"), `LOG='`+bwlog+`'
{ printf 'ARGS:'; for a in "$@"; do printf ' [%s]' "$a"; done; echo; env | sort | sed 's/^/ENV /'; } >> "$LOG"
case "$1" in
unlock)
  if IFS= read -r line; then echo "UNEXPECTED STDIN" >> "$LOG"; fi
  if [ "$ICEBATH_BW_MASTER" = "master" ]; then echo "bwtok"; else echo "You are not logged in." >&2; exit 1; fi ;;
list)
  cat <<'EOF'
[{"id":"i1","type":1,"name":"Bank","creationDate":"2024","login":{"username":" bob ","uris":[{"uri":"https://bank.example/login","match":null},{"uri":"https://never.example","match":5},{"uri":"androidapp://bank"},{"uri":null},{"uri":"not a url"}]}},
 {"id":"card","type":3,"name":"Card","login":null},
 {"id":"i2","type":1,"name":"","login":{"uris":[{"uri":"androidapp://only"}]}}]
EOF
  ;;
get)
  case "$2" in password) printf 'bw pass\r\n' ;; totp) echo "No TOTP available for this login." ;; esac ;;
esac
`)
	os.Setenv("OPENAI_API_KEY", "canary-should-not-pass")
	os.Setenv("OP_CONNECT_TOKEN", "launch-connect-should-not-pass")
	defer os.Unsetenv("OPENAI_API_KEY")
	defer os.Unsetenv("OP_CONNECT_TOKEN")
	secrets := map[string]string{"OP_CONNECT_HOST": "https://connect.example"}
	v := vault.New(home.New(hostinfo.New()), vault.NewKeychain(), func(_ context.Context, name string) string { return secrets[name] })
	opts := vault.Options{OnePassword: vault.OnePasswordOptions{BinaryPath: filepath.Join(bin, "op"), Account: "my"},
		Bitwarden: vault.BitwardenOptions{BinaryPath: filepath.Join(bin, "bw")}}
	backends, err := v.Backends(ctx, opts)
	must(err, "backends")
	names := []string{}
	for _, b := range backends {
		names = append(names, b.Name())
	}
	check(strings.Join(names, ",") == "local,onepassword,bitwarden", "backends %v", names)
	sources, _ := v.Sources(ctx, opts)
	sj, _ := json.Marshal(sources)
	check(string(sj) == `[{"name":"local","display_name":"icebath vault","enabled":true,"needs_unlock":false,"unlocked":true,"installed":true},{"name":"onepassword","display_name":"1Password","enabled":true,"needs_unlock":true,"unlocked":false,"installed":true},{"name":"bitwarden","display_name":"Bitwarden","enabled":true,"needs_unlock":true,"unlocked":false,"installed":true}]`, "sources %s", sj)
	op, bw := backends[1], backends[2]
	items, err := op.List(ctx)
	check(err == nil && len(items) == 0, "locked 1Password lists nothing")
	_, err = op.ResolvePassword(ctx, "op:abc")
	var unlockReq *vault.UnlockRequiredError
	check(errors.As(err, &unlockReq), "locked resolve asks for unlock %v", err)
	err = op.(vault.Unlocker).Unlock(ctx, "wrong")
	check(err != nil && err.Error() == "1Password unlock failed: [ERROR] invalid master password", "wrong master %v", err)
	err = op.(vault.Unlocker).Unlock(ctx, "master")
	check(err == nil && op.Unlocked(ctx), "unlock %v", err)
	items, err = op.List(ctx)
	ij, _ := json.Marshal(items)
	check(err == nil && string(ij) == `[{"id":"op:abc","kind":"login","label":"Example","origin":"https://example.com","created_at":"2024-01-01T00:00:00Z","identifier":"alice@example.com","identifier_type":"username","allowed_origins":["https://example.com","https://example.org"]},{"id":"op:appOnly","kind":"login","label":"androidapp://x","origin":"androidapp://x","created_at":""}]`,
		"1Password items %s %v", ij, err)
	pw, err := op.ResolvePassword(ctx, "op:abc")
	check(err == nil && pw.Reveal(vault.PasswordField) == "pa ss", "1Password password trims only the trailing newline %q", pw.Reveal(vault.PasswordField))
	code, err := op.ResolveOTP(ctx, "op:abc")
	check(err == nil && code.Reveal(vault.CodeField) == "123456", "1Password code")
	_, err = op.ResolvePassword(ctx, "op:unknown")
	check(err != nil && err.Error() == "1Password login not found; list logins again", "unlisted handle %v", err)
	os.WriteFile(opExpired, nil, 0o600)
	_, err = op.List(ctx)
	check(errors.As(err, &unlockReq) && !op.Unlocked(ctx), "expired session locks 1Password %v", err)
	os.Remove(opExpired)
	logText := string(mustRead(oplog))
	check(strings.Contains(logText, "ARGS: [signin] [--raw] [--account] [my]") && strings.Contains(logText, "STDIN [master]") &&
		strings.Contains(logText, "ARGS: [item] [list] [--categories] [Login] [--format] [json]") &&
		strings.Contains(logText, "ARGS: [item] [get] [abc] [--vault] [v1] [--fields] [label=password] [--reveal]") &&
		strings.Contains(logText, "ARGS: [item] [get] [abc] [--vault] [v1] [--otp]"), "1Password command lines")
	check(strings.Contains(logText, "ENV OP_SESSION_my=tok-123") && strings.Contains(logText, "ENV NO_COLOR=1") &&
		strings.Contains(logText, "ENV OP_ACCOUNT=my") && strings.Contains(logText, "ENV OP_CONNECT_HOST=https://connect.example") &&
		!strings.Contains(logText, "canary") && !strings.Contains(logText, "launch-connect") && !strings.Contains(logText, "ICEBATH_HOME"),
		"1Password environment is the allowlist plus the profile's connect host and the session")
	check(!strings.Contains(logText, "master]") || strings.Count(logText, "master") == 2, "master password only on stdin")

	os.WriteFile(opSlow, nil, 0o600)
	done := make(chan error, 1)
	go func() { done <- op.(vault.Unlocker).Unlock(ctx, "master") }()
	time.Sleep(700 * time.Millisecond)
	v.Lock(ctx, "onepassword")
	err = <-done
	check(err != nil && err.Error() == "1Password was locked while unlocking; try again" && !op.Unlocked(ctx), "lock during unlock wins %v", err)
	os.Remove(opSlow)

	err = bw.(vault.Unlocker).Unlock(ctx, "wrong")
	check(err != nil && err.Error() == "Bitwarden unlock failed: not logged in — run `bw login` once in a terminal first", "bw wrong %v", err)
	err = bw.(vault.Unlocker).Unlock(ctx, "master")
	check(err == nil, "bw unlock %v", err)
	items, err = bw.List(ctx)
	ij, _ = json.Marshal(items)
	check(err == nil && string(ij) == `[{"id":"bw:i1","kind":"login","label":"Bank","origin":"https://bank.example","created_at":"2024","identifier":"bob","identifier_type":"username"},{"id":"bw:i2","kind":"login","label":"androidapp://only","origin":"androidapp://only","created_at":""}]`,
		"Bitwarden items %s %v", ij, err)
	bpw, err := bw.ResolvePassword(ctx, "bw:i1")
	check(err == nil && bpw.Reveal(vault.PasswordField) == "bw pass", "bw password")
	bcode, err := bw.ResolveOTP(ctx, "bw:i1")
	check(err == nil && len(bcode.Fields()) == 0, "bw without totp gives no code")
	bl := string(mustRead(bwlog))
	check(strings.Contains(bl, "ARGS: [unlock] [--raw] [--nointeraction] [--passwordenv] [ICEBATH_BW_MASTER]") &&
		strings.Contains(bl, "ENV ICEBATH_BW_MASTER=master") && strings.Contains(bl, "ARGS: [list] [items] [--nointeraction]") &&
		strings.Contains(bl, "ARGS: [get] [password] [i1] [--nointeraction]") && strings.Contains(bl, "ENV BW_SESSION=bwtok") &&
		!strings.Contains(bl, "UNEXPECTED STDIN") && !strings.Contains(bl, "canary"), "Bitwarden command lines and environment")
	check(strings.Count(bl, "ICEBATH_BW_MASTER=") == 2, "master password variable only on unlock calls")
	v.Lock(ctx, "")
	check(!bw.Unlocked(ctx) && !op.Unlocked(ctx), "lock all")
	disabled, _ := v.Backends(ctx, vault.Options{OnePassword: vault.OnePasswordOptions{Disabled: true, BinaryPath: filepath.Join(bin, "op")},
		Bitwarden: vault.BitwardenOptions{BinaryPath: filepath.Join(root, "missing-bw")}})
	check(len(disabled) == 1, "disabled and missing managers are not sources")
	tok := vault.New(home.New(hostinfo.New()), vault.NewKeychain(), func(_ context.Context, name string) string {
		if name == "MY_TOKEN" {
			return "svc"
		}
		return ""
	})
	sb, _ := tok.Backends(ctx, vault.Options{OnePassword: vault.OnePasswordOptions{BinaryPath: filepath.Join(bin, "op"), ServiceAccountTokenEnv: "MY_TOKEN"}})
	check(len(sb) >= 2 && sb[1].Unlocked(ctx), "service account token counts as unlocked")
	os.WriteFile(opExpired, nil, 0o600)
	_, err = sb[1].List(ctx)
	check(err != nil && strings.HasPrefix(err.Error(), "op failed: [ERROR] 2024/01/01 session expired") && sb[1].Unlocked(ctx), "service account auth failure is op's own error %v", err)
	os.Remove(opExpired)
	check(strings.Contains(string(mustRead(oplog)), "ENV OP_SERVICE_ACCOUNT_TOKEN=svc"), "service account token reaches op")
}

type probePlugin struct {
	sawValue bool
	mode     string
}

func (p *probePlugin) List(ctx context.Context) ([]vault.ItemMeta, error) {
	_, prompt := vault.PrompterFrom(ctx)
	p.sawValue = prompt || home.Override(ctx) != ""
	return []vault.ItemMeta{{ID: "pp:1", Kind: "login", Label: "One", Origin: "https://p.example"}, {ID: "vault_spoof", Kind: "login"}}, nil
}
func (p *probePlugin) Meta(ctx context.Context, h string) (vault.ItemMeta, bool, error) {
	return vault.ItemMeta{ID: "pp:other", Kind: "login"}, true, nil
}
func (p *probePlugin) ResolvePassword(ctx context.Context, h string) (string, error) {
	return "", errors.New("failed with secret hunter2")
}
func (p *probePlugin) ResolveOTP(ctx context.Context, h string) (string, error) { panic("plugin crash hunter2") }
func (p *probePlugin) ResolveSecret(ctx context.Context, h string) ([]vault.Field, error) {
	return []vault.Field{{Name: "password", Value: "x"}}, nil
}
func (p *probePlugin) Fill(ctx context.Context, fill func(context.Context) error) error {
	switch p.mode {
	case "skip":
		return nil
	case "twice":
		_ = fill(ctx)
		return fill(ctx)
	case "fail":
		_ = fill(ctx)
		return errors.New("exit failed hunter2")
	}
	return fill(ctx)
}

type nopPrompter struct{}

func (nopPrompter) MasterPassword(context.Context, string, string) (string, error)     { return "", nil }
func (nopPrompter) SaveLogin(context.Context, string, string) (vault.Login, bool, error) { return vault.Login{}, false, nil }
func (nopPrompter) OneTimeCode(context.Context, string, string) (string, error)        { return "", nil }

func pluginTests(ctx context.Context) {
	fmt.Println("== plugin backends")
	v := newVault()
	pl := &probePlugin{}
	for _, bad := range []string{"local", "op", "vault", "bw", "Bad", "1x", "vault_x", "onepassword"} {
		_, err := v.RegisterPlugin(ctx, "", bad, pl)
		check(err != nil, "name %q refused %v", bad, err)
	}
	unreg, err := v.RegisterPlugin(ctx, "", "pp", pl)
	must(err, "register")
	_, err = v.RegisterPlugin(ctx, "", "pp", pl)
	check(err != nil && err.Error() == "login backend name 'pp' is already registered", "duplicate %v", err)
	bs, _ := v.Backends(ctx, vault.Options{})
	check(len(bs) == 1, "plugin off until enabled")
	opts := vault.Options{Plugins: map[string]bool{"pp": true}}
	bs, _ = v.Backends(ctx, opts)
	check(len(bs) == 2 && bs[1].Name() == "pp" && bs[1].DisplayName() == "pp (plugin)" && !bs[1].NeedsUnlock(), "plugin wrapper")
	p := bs[1]
	pctx := vault.WithPrompter(home.WithOverride(ctx, homeDir()), nopPrompter{})
	items, err := p.List(pctx)
	check(err == nil && len(items) == 1 && items[0].ID == "pp:1" && !pl.sawValue, "plugin list filtered and sees no caller values %v", items)
	_, found, _ := p.Meta(ctx, "pp:1")
	check(!found, "item claiming another id is not found")
	_, err = p.ResolvePassword(ctx, "pp:1")
	check(err != nil && err.Error() == "The pp login backend failed.", "plugin error replaced %v", err)
	_, err = p.ResolveOTP(ctx, "pp:1")
	check(err != nil && err.Error() == "The pp login backend failed.", "plugin panic replaced %v", err)
	_, err = p.ResolvePassword(ctx, "vault_x")
	check(err != nil && err.Error() == "Handle is outside the pp login backend's namespace.", "outside %v", err)
	calls := 0
	fill := func(context.Context) error { calls++; return nil }
	pl.mode = ""
	err = p.Fill(ctx, fill)
	check(err == nil && calls == 1, "fill runs once")
	pl.mode = "skip"
	err = p.Fill(ctx, fill)
	check(err != nil && err.Error() == "The pp login backend failed." && calls == 1, "plugin skipping the fill is a failure %v", err)
	pl.mode = "twice"
	err = p.Fill(ctx, fill)
	check(err == nil && calls == 2, "fill never runs twice (%d)", calls)
	pl.mode = "fail"
	err = p.Fill(ctx, fill)
	check(err != nil && err.Error() == "The pp login backend failed." && calls == 3, "plugin exit failure replaces the result %v", err)
	unreg()
	bs, _ = v.Backends(ctx, opts)
	check(len(bs) == 1, "unregistered")
}

func sessionScopeTests(ctx context.Context, root string) {
	fmt.Println("== session scoping")
	bin := filepath.Join(root, "bin")
	v := newVault()
	opts := vault.Options{Bitwarden: vault.BitwardenOptions{BinaryPath: filepath.Join(bin, "bw")}, OnePassword: vault.OnePasswordOptions{Disabled: true}}
	a := home.WithOverride(ctx, filepath.Join(root, "profA"))
	b := home.WithOverride(ctx, filepath.Join(root, "profB"))
	bwOf := func(c context.Context) vault.LoginBackend {
		bs, err := v.Backends(c, opts)
		must(err, "backends")
		return bs[1]
	}
	err := bwOf(vault.WithSession(a, "s1")).(vault.Unlocker).Unlock(vault.WithSession(a, "s1"), "master")
	must(err, "unlock A")
	check(bwOf(a).Unlocked(a) && !bwOf(b).Unlocked(b), "an unlock belongs to its profile only")
	v.Release("s2")
	check(bwOf(a).Unlocked(a), "another session ending keeps the unlock")
	v.Release("s1")
	check(!bwOf(a).Unlocked(a), "the unlocking session ending locks it")
	check(!vault.CanPrompt(ctx) && vault.CanPrompt(vault.WithPrompter(ctx, nopPrompter{})) &&
		!vault.CanPrompt(vault.Unattended(vault.WithPrompter(ctx, nopPrompter{}))), "prompting needs a prompter and an attended context")
}

func secretServiceTest(ctx context.Context, root string) {
	fmt.Println("== Secret Service (own throwaway item only)")
	conn, err := dbus.Connect(os.Getenv("DBUS_SESSION_BUS_ADDRESS"))
	if err != nil {
		fmt.Println("info session bus unreachable:", err)
		return
	}
	defer conn.Close()
	var owned bool
	if err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, "org.freedesktop.secrets").Store(&owned); err != nil || !owned {
		fmt.Println("info no running Secret Service, test not run", err)
		return
	}
	col := conn.Object("org.freedesktop.secrets", "/org/freedesktop/secrets/aliases/default")
	if _, err := col.GetProperty("org.freedesktop.Secret.Collection.Label"); err != nil {
		fmt.Println("info no default collection, test not run:", err)
		return
	}
	lv, err := col.GetProperty("org.freedesktop.Secret.Collection.Locked")
	if locked, _ := lv.Value().(bool); err != nil || locked {
		fmt.Println("info default collection locked or unreadable, test not run", err)
		return
	}
	h := filepath.Join(root, "sshome")
	hctx := home.WithOverride(ctx, h)
	v := newVault()
	st, _ := v.Store(hctx, vault.Options{KeyStorage: "keychain"})
	_, _, refPath := vaultPaths(h)
	kc := vault.NewKeychain()
	defer func() {
		if loc, err := st.KeyLocation(); err == nil && loc.Account != "" {
			_ = kc.Delete(ctx, loc.Service, loc.Account)
		}
		if raw, err := os.ReadFile(refPath); err == nil {
			var r struct{ Service, Account string }
			if json.Unmarshal(raw, &r) == nil && strings.HasPrefix(r.Account, "vault-") {
				_ = kc.Delete(ctx, r.Service, r.Account)
			}
		}
	}()
	check(kc.Check(ctx) == nil && kc.Label(ctx) == "Secret Service", "Secret Service reachable")
	m, err := st.Add(hctx, "login", "ss", loginPayload("a", "keychain-pw"), "https://ss.example")
	must(err, "add keychain vault")
	data, keyPath, _ := vaultPaths(h)
	_, kerr := os.Stat(keyPath)
	refRaw := mustRead(refPath)
	var ref struct{ Storage, Service, Account, Vault_dir string }
	json.Unmarshal(refRaw, &ref)
	resolved, _ := filepath.EvalSymlinks(filepath.Join(h, "vault"))
	wantRef := fmt.Sprintf("{\n  \"storage\": \"keychain\",\n  \"service\": \"icebath vault key\",\n  \"account\": \"%s\",\n  \"vault_dir\": \"%s\"\n}", ref.Account, resolved)
	check(errors.Is(kerr, fs.ErrNotExist) && string(refRaw) == wantRef && len(ref.Account) == 38, "key in the keychain, pointer bytes %q", refRaw)
	keyText, err := kc.Get(ctx, "icebath vault key", ref.Account)
	check(err == nil && len(keyText) == 44, "own entry reads back through the encrypted session")
	plain, _, _, err := pfDecrypt(keyText, string(mustRead(data)))
	check(err == nil && strings.Contains(string(plain), m.ID), "keychain key opens the vault")
	attrs := itemAttributes(conn, "icebath vault key", ref.Account)
	check(attrs["xdg:schema"] == "org.freedesktop.Secret.Generic" && attrs["application"] == "icebath" && attrs["username"] == ref.Account, "item attributes %v", attrs)
	ms, err := listErrCtx(hctx, v)
	check(err == nil && len(ms) == 1, "reopened vault lists through the keychain key")
	out, err := st.MigrateKey(hctx, "file")
	check(err == nil && out == "file", "migrate to file %v", err)
	_, rerr := os.Stat(refPath)
	check(string(mustRead(keyPath)) == keyText && errors.Is(rerr, fs.ErrNotExist), "key moved to the file and pointer removed")
	_, err = kc.Get(ctx, "icebath vault key", ref.Account)
	check(errors.Is(err, vault.ErrNotFound), "keychain entry deleted after the move %v", err)
	err = kc.Delete(ctx, "icebath vault key", ref.Account)
	check(errors.Is(err, vault.ErrNotFound), "deleting again reports not found")
	_, err = kc.Get(ctx, "bad\x00name", "x")
	check(err != nil, "NUL in a name refused")
}

func itemAttributes(conn *dbus.Conn, service, account string) map[string]string {
	var items []dbus.ObjectPath
	col := conn.Object("org.freedesktop.secrets", "/org/freedesktop/secrets/aliases/default")
	if err := col.Call("org.freedesktop.Secret.Collection.SearchItems", 0, map[string]string{"service": service, "username": account}).Store(&items); err != nil || len(items) != 1 {
		return nil
	}
	v, err := conn.Object("org.freedesktop.secrets", items[0]).GetProperty("org.freedesktop.Secret.Item.Attributes")
	if err != nil {
		return nil
	}
	m, _ := v.Value().(map[string]string)
	return m
}
