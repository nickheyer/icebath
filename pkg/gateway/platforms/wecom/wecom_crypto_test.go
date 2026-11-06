package wecom

import (
	"encoding/xml"
	"strings"
	"testing"
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
