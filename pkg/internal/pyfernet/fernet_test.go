package pyfernet

import (
	"errors"
	"testing"
)

func TestFernetSpecVectorAndRoundTrip(t *testing.T) {
	f, err := New([]byte("cw_0x689RpI-jtRR7oE8h_eQsKImvJapLeSbXpwF4e4="))
	if err != nil {
		t.Fatal(err)
	}
	iv := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	want := "gAAAAAAdwJ6wAAECAwQFBgcICQoLDA0ODy021cpGVWKZ_eEwCGM4BLLF_5CV9dOPmrhuVUPgJobwOz7JcbmrR64jVmpU4IwqDA=="
	if got := string(f.EncryptAtTime([]byte("hello"), 499162800, iv)); got != want {
		t.Fatalf("token %s", got)
	}
	plain, err := f.Decrypt([]byte(want))
	if err != nil || string(plain) != "hello" {
		t.Fatalf("decrypt %q %v", plain, err)
	}
	other, _ := New(GenerateKey())
	if _, err := other.Decrypt([]byte(want)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong key %v", err)
	}
	for _, bad := range []string{"", "not base64!", "gAAAAAAdwJ6w", want[:len(want)-8] + "AAAAAAA="} {
		if _, err := f.Decrypt([]byte(bad)); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("%q decrypted %v", bad, err)
		}
	}
	if _, err := New([]byte("short")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("short key %v", err)
	}
	if got, _ := f.Decrypt(f.Encrypt([]byte{})); len(got) != 0 {
		t.Fatalf("empty round trip %q", got)
	}
}
