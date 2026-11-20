package wecom

import (
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

func TestWecomCryptoRoundtripEncryptDecrypt(t *testing.T) {
	crypt, err := NewWXBizMsgCrypt("test-callback-token", "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG", "ww1234567890")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := crypt.Encrypt("<xml><Content>hello</Content></xml>", "nonce123", "123456")
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Encrypt, MsgSignature, TimeStamp, Nonce string
	}
	if err := xml.Unmarshal([]byte(encrypted), &root); err != nil {
		t.Fatal(err)
	}
	decrypted, err := crypt.Decrypt(root.MsgSignature, root.TimeStamp, root.Nonce, root.Encrypt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decrypted), "<Content>hello</Content>") {
		t.Fatalf("decrypted %q", decrypted)
	}
	if _, err := crypt.Decrypt("bad", root.TimeStamp, root.Nonce, root.Encrypt); err == nil || err.Error() != "signature mismatch" {
		t.Fatalf("signature err = %v", err)
	}
	other, _ := NewWXBizMsgCrypt("test-callback-token", "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG", "ww0")
	if _, err := other.Decrypt(root.MsgSignature, root.TimeStamp, root.Nonce, root.Encrypt); err == nil || err.Error() != "receive_id mismatch" {
		t.Fatalf("receive id err = %v", err)
	}
	if _, err := NewWXBizMsgCrypt("t", "short", "r"); err == nil || err.Error() != "encoding_aes_key must be 43 chars" {
		t.Fatalf("key err = %v", err)
	}
	if _, err := PKCS7Decode([]byte{1, 2, 3, 2}); err == nil || err.Error() != "malformed PKCS7 padding" {
		t.Fatalf("padding err = %v", err)
	}
}

// Failure of the encrypt side the tests drive
type EncryptError struct{ Msg string }

func (e *EncryptError) Error() string   { return e.Msg }
func (*EncryptError) weComCryptoError() {}

// PKCS7Encode pads to a 32 byte block, adding a full block when already aligned
func PKCS7Encode(text []byte) []byte {
	amount := pkcs7BlockSize - len(text)%pkcs7BlockSize
	if amount == 0 {
		amount = pkcs7BlockSize
	}
	return append(append([]byte{}, text...), bytes.Repeat([]byte{byte(amount)}, amount)...)
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Encrypt seals the plaintext and renders the signed Encrypt, MsgSignature, TimeStamp and Nonce XML envelope
func (c *WXBizMsgCrypt) Encrypt(plaintext, nonce, timestamp string) (string, error) {
	if nonce == "" {
		n, err := randomNonce(10)
		if err != nil {
			return "", err
		}
		nonce = n
	}
	if timestamp == "" {
		timestamp = strconv.FormatInt(time.Now().Unix(), 10)
	}
	if err := pystr.StrictUTF8EncodeError(plaintext); err != nil {
		return "", err
	}
	encrypt, err := c.encryptBytes([]byte(plaintext))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("<xml>")
	for _, kv := range [][2]string{{"Encrypt", encrypt}, {"MsgSignature", sha1Signature(c.Token, timestamp, nonce, encrypt)}, {"TimeStamp", timestamp}, {"Nonce", nonce}} {
		fmt.Fprintf(&b, "<%s>%s</%s>", kv[0], xmlEscape(kv[1]), kv[0])
	}
	b.WriteString("</xml>")
	return b.String(), nil
}

func (c *WXBizMsgCrypt) encryptBytes(raw []byte) (string, error) {
	fail := func(err error) (string, error) { return "", &EncryptError{fmt.Sprintf("encrypt failed: %v", err)} }
	prefix := make([]byte, 16)
	if _, err := rand.Read(prefix); err != nil {
		return fail(err)
	}
	if uint64(len(raw)) > 0xFFFFFFFF {
		return fail(fmt.Errorf("int larger than 32 bits"))
	}
	payload := append(prefix, binary.BigEndian.AppendUint32(nil, uint32(len(raw)))...)
	payload = append(append(payload, raw...), c.ReceiveID...)
	block, err := c.block()
	if err != nil {
		return fail(err)
	}
	padded := PKCS7Encode(payload)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, c.IV).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Random nonce over digits and ASCII letters
func randomNonce(length int) (string, error) {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	out := make([]byte, length)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}
