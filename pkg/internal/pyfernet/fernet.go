package pyfernet

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
)

// InvalidToken cryptography raises for a malformed, forged or undecryptable token
var ErrInvalidToken = errors.New("InvalidToken")

// ValueError Fernet raises for a key that is not 32 url safe base64 bytes
var ErrInvalidKey = errors.New("Fernet key must be 32 url-safe base64-encoded bytes.")

type Fernet struct {
	signing, encryption []byte
}

// Url safe base64 of 32 random bytes, padded as cryptography emits it
func GenerateKey() []byte {
	raw := make([]byte, 32)
	rand.Read(raw)
	return []byte(base64.URLEncoding.EncodeToString(raw))
}

// Lenient urlsafe_b64decode that discards characters outside the alphabet as CPython does
func urlsafeDecode(b []byte) ([]byte, error) {
	s := strings.NewReplacer("-", "+", "_", "/").Replace(string(b))
	return pybase64.B64Decode(s)
}

// Fernet over a url safe base64 key, the error matching the ValueError cryptography raises
func New(key []byte) (*Fernet, error) {
	raw, err := urlsafeDecode(key)
	if err != nil || len(raw) != 32 {
		return nil, ErrInvalidKey
	}
	return &Fernet{signing: raw[:16], encryption: raw[16:]}, nil
}

// Token for data stamped with the current time and a random IV
func (f *Fernet) Encrypt(data []byte) []byte {
	iv := make([]byte, aes.BlockSize)
	rand.Read(iv)
	return f.EncryptAtTime(data, time.Now().Unix(), iv)
}

// Token for data with an explicit timestamp and IV, the spec vector form
func (f *Fernet) EncryptAtTime(data []byte, at int64, iv []byte) []byte {
	pad := aes.BlockSize - len(data)%aes.BlockSize
	padded := append(append([]byte{}, data...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(f.encryption)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	msg := make([]byte, 0, 25+len(ct)+32)
	msg = append(msg, 0x80)
	msg = binary.BigEndian.AppendUint64(msg, uint64(at))
	msg = append(msg, iv...)
	msg = append(msg, ct...)
	mac := hmac.New(sha256.New, f.signing)
	mac.Write(msg)
	msg = mac.Sum(msg)
	return []byte(base64.URLEncoding.EncodeToString(msg))
}

// Plaintext of a token without a ttl check, ErrInvalidToken for every failure cryptography folds into it
func (f *Fernet) Decrypt(token []byte) ([]byte, error) {
	data, err := urlsafeDecode(token)
	if err != nil || len(data) < 9 || data[0] != 0x80 {
		return nil, ErrInvalidToken
	}
	if len(data) < 25+32 {
		return nil, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, f.signing)
	mac.Write(data[:len(data)-32])
	if !hmac.Equal(mac.Sum(nil), data[len(data)-32:]) {
		return nil, ErrInvalidToken
	}
	iv, ct := data[9:25], data[25:len(data)-32]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, ErrInvalidToken
	}
	block, _ := aes.NewCipher(f.encryption)
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ct)
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > aes.BlockSize {
		return nil, ErrInvalidToken
	}
	for _, b := range plain[len(plain)-pad:] {
		if int(b) != pad {
			return nil, ErrInvalidToken
		}
	}
	return plain[:len(plain)-pad], nil
}
