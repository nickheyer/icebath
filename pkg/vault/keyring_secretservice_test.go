package vault

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// In process Secret Service daemon speaking the wire protocol, a locked collection unlocking through a prompt
type fakeSecretService struct {
	mu       sync.Mutex
	locked   bool
	aesKey   []byte
	items    map[dbusObjectPath]map[string]string
	secrets  map[dbusObjectPath][]byte
	next     int
	prompted int
}

func (f *fakeSecretService) serve(t *testing.T, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go f.handle(t, conn)
	}
}

func (f *fakeSecretService) handle(t *testing.T, conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	if line, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(line, "\x00AUTH EXTERNAL ") {
		return
	}
	fmt.Fprint(conn, "OK 0123456789abcdef\r\n")
	if line, _ := r.ReadString('\n'); line != "BEGIN\r\n" {
		return
	}
	var serial uint32 = 100
	send := func(m *dbusMessage) {
		serial++
		m.Serial = serial
		raw, err := m.marshal()
		if err != nil {
			t.Errorf("server marshal %v", err)
			return
		}
		conn.Write(raw)
	}
	for {
		m, err := dbusReadMessage(r)
		if err != nil {
			return
		}
		reply := &dbusMessage{Type: dbusMethodReturn, ReplySerial: m.Serial}
		var after *dbusMessage
		f.mu.Lock()
		switch m.Member {
		case "Hello":
			reply.Signature, reply.Body = "s", []any{":1.7"}
		case "NameHasOwner":
			reply.Signature, reply.Body = "b", []any{m.Body[0] == ssBusName}
		case "AddMatch":
		case "Get":
			if m.Body[1] == "Label" {
				reply.Signature, reply.Body = "v", []any{dbusVariant{"s", "Login"}}
			} else {
				_, isItem := f.items[m.Path]
				reply.Signature, reply.Body = "v", []any{dbusVariant{"b", f.locked && !isItem}}
			}
		case "Unlock":
			prompt := dbusObjectPath("/")
			if f.locked {
				prompt = "/org/freedesktop/secrets/prompt/u1"
			}
			reply.Signature, reply.Body = "aoo", []any{[]any{}, prompt}
		case "Prompt":
			f.prompted++
			f.locked = false
			after = &dbusMessage{Type: dbusSignal, Path: m.Path, Interface: ssPromptIface, Member: "Completed",
				Signature: "bv", Body: []any{false, dbusVariant{"ao", []any{ssDefaultColl}}}}
		case "OpenSession":
			clientPub := new(big.Int).SetBytes(m.Body[1].(dbusVariant).Val.([]byte))
			privBytes := make([]byte, 0x80)
			rand.Read(privBytes)
			priv := new(big.Int).SetBytes(privBytes)
			pub := new(big.Int).Exp(big.NewInt(2), priv, ssDHPrime)
			common := new(big.Int).Exp(clientPub, priv, ssDHPrime).Bytes()
			common = append(bytes.Repeat([]byte{0}, 0x80-len(common)), common...)
			prk := hmac.New(sha256.New, make([]byte, 0x20))
			prk.Write(common)
			okm := hmac.New(sha256.New, prk.Sum(nil))
			okm.Write([]byte{1})
			f.aesKey = okm.Sum(nil)[:16]
			reply.Signature, reply.Body = "vo", []any{dbusVariant{"ay", pub.Bytes()}, dbusObjectPath("/org/freedesktop/secrets/session/s1")}
		case "SearchItems":
			want := map[string]string{}
			for _, e := range m.Body[0].([]dbusEntry) {
				want[e.K.(string)] = e.V.(string)
			}
			found := []any{}
			for path, attrs := range f.items {
				if attrs["service"] == want["service"] && attrs["username"] == want["username"] {
					found = append(found, path)
				}
			}
			reply.Signature, reply.Body = "ao", []any{found}
		case "CreateItem":
			attrs := map[string]string{}
			for _, e := range m.Body[0].([]dbusEntry) {
				if e.K == "org.freedesktop.Secret.Item.Attributes" {
					for _, a := range e.V.(dbusVariant).Val.([]dbusEntry) {
						attrs[a.K.(string)] = a.V.(string)
					}
				}
			}
			secret := m.Body[1].([]any)
			block, _ := aes.NewCipher(f.aesKey)
			ct := secret[2].([]byte)
			plain := make([]byte, len(ct))
			cipher.NewCBCDecrypter(block, secret[1].([]byte)).CryptBlocks(plain, ct)
			plain = plain[:len(plain)-int(plain[len(plain)-1])]
			path := dbusObjectPath("")
			for p, a := range f.items {
				if a["service"] == attrs["service"] && a["username"] == attrs["username"] {
					path = p
				}
			}
			if path == "" {
				f.next++
				path = dbusObjectPath(fmt.Sprintf("/org/freedesktop/secrets/collection/login/%d", f.next))
			}
			f.items[path], f.secrets[path] = attrs, plain
			reply.Signature, reply.Body = "oo", []any{path, dbusObjectPath("/")}
		case "GetSecret":
			plain := f.secrets[m.Path]
			pad := 16 - len(plain)%16
			padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
			iv := make([]byte, 16)
			rand.Read(iv)
			block, _ := aes.NewCipher(f.aesKey)
			ct := make([]byte, len(padded))
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
			reply.Signature, reply.Body = "(oayays)", []any{[]any{m.Body[0], iv, ct, "text/plain"}}
		case "Delete":
			delete(f.items, m.Path)
			delete(f.secrets, m.Path)
			reply.Signature, reply.Body = "o", []any{dbusObjectPath("/")}
		default:
			reply = &dbusMessage{Type: dbusErrorReply, ReplySerial: m.Serial, ErrorName: "org.freedesktop.DBus.Error.UnknownMethod",
				Signature: "s", Body: []any{"no " + m.Member}}
		}
		f.mu.Unlock()
		send(reply)
		if after != nil {
			send(after)
		}
	}
}

func TestSecretServiceKeyringRoundTripsThroughTheWireProtocol(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "bus")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fake := &fakeSecretService{locked: true, items: map[dbusObjectPath]map[string]string{}, secrets: map[dbusObjectPath][]byte{}}
	go fake.serve(t, ln)
	ss := secretServiceKeyring{dial: func() (*dbusConn, error) { return dbusDial("unix:path=" + sock + ",guid=x") }}
	if err := ss.viable(); err != nil {
		t.Fatalf("viable %v", err)
	}
	if keyringName(ss) != "SecretService Keyring" || !isOSKeychain(ss) {
		t.Fatalf("name %q", keyringName(ss))
	}
	key := "bm90LXRoZS1rZXktbm90LXRoZS1rZXktbm90LXRoZS1rZXk="
	if err := ss.SetPassword(VaultKeychainService, "vault-abc", key); err != nil {
		t.Fatalf("set %v", err)
	}
	if fake.prompted != 1 {
		t.Fatalf("locked collection must unlock through its prompt, prompted %d", fake.prompted)
	}
	got, ok, err := ss.GetPassword(VaultKeychainService, "vault-abc")
	if err != nil || !ok || got != key {
		t.Fatalf("get %q %v %v", got, ok, err)
	}
	for _, attrs := range fake.items {
		if attrs["application"] != ssAppID {
			t.Fatalf("attributes %v", attrs)
		}
	}
	if _, ok, err := ss.GetPassword(VaultKeychainService, "vault-other"); ok || err != nil {
		t.Fatalf("missing entry %v %v", ok, err)
	}
	if err := ss.DeletePassword(VaultKeychainService, "vault-abc"); err != nil {
		t.Fatalf("delete %v", err)
	}
	err = ss.DeletePassword(VaultKeychainService, "vault-abc")
	if ke, ok := err.(*keyringError); !ok || ke.Class != "PasswordDeleteError" {
		t.Fatalf("second delete %v", err)
	}
}

func TestDBusMarshalRoundTripsNestedTypes(t *testing.T) {
	m := &dbusMessage{Type: dbusMethodCall, Serial: 9, Path: "/a/b", Member: "X", Destination: "d.e",
		Signature: "a{sv}(oayays)bxda{ss}",
		Body: []any{[]dbusEntry{{"k", dbusVariant{"as", []any{"p", "q"}}}}, []any{dbusObjectPath("/s"), []byte{1, 2}, []byte{}, "t"},
			true, int64(-5), 2.5, map[string]string{"b": "2", "a": "1"}}}
	raw, err := m.marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := dbusReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if back.Path != "/a/b" || back.Member != "X" || back.Signature != m.Signature || back.Serial != 9 {
		t.Fatalf("header %+v", back)
	}
	got := fmt.Sprint(back.Body)
	want := "[[{k {as [p q]}}] [/s [1 2] [] t] true -5 2.5 [{a 1} {b 2}]]"
	if got != want {
		t.Fatalf("body %s", got)
	}
	if addr, _ := dbusSocketAddress("tcp:host=x;unix:abstract=/tmp/dbus-1%2c2,guid=1"); addr != "@/tmp/dbus-1,2" {
		t.Fatalf("address %q", addr)
	}
}
