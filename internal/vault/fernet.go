package vault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"
)

// Version byte every Fernet token opens with
const fernetVersion = 0x80

// Shortest token the format allows, version, timestamp, IV, one cipher block and the HMAC
const fernetMinToken = 1 + 8 + aes.BlockSize + aes.BlockSize + sha256.Size

// Error for a token that is malformed, forged or encrypted under another key
var errInvalidToken = errors.New("invalid Fernet token")

// Error for base64 text whose data characters leave a partial quantum
var errBase64 = errors.New("incorrect base64 padding")

// Signing and encryption halves of a Fernet key
type fernetKey struct {
	signing    []byte
	encryption []byte
}

// Url-safe padded base64 of 32 fresh random bytes, the form a vault key is stored in
func newFernetKey() ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	return []byte(base64.URLEncoding.EncodeToString(raw)), nil
}

// Key from its stored text, refused unless the lenient decode yields exactly 32 bytes
func parseFernetKey(text []byte) (fernetKey, error) {
	raw, err := decodeBase64Lenient(text)
	if err != nil || len(raw) != 32 {
		return fernetKey{}, failure(ErrBadKey, "Fernet key must be 32 url-safe base64-encoded bytes.")
	}
	return fernetKey{signing: raw[:16], encryption: raw[16:]}, nil
}

// Token of plain stamped with the whole seconds of at and a random IV, PKCS7 padded and HMAC signed
func (k fernetKey) encrypt(plain []byte, at time.Time) ([]byte, error) {
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k.encryption)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(bytes.Clone(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	sealed := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(sealed, padded)
	msg := binary.BigEndian.AppendUint64([]byte{fernetVersion}, uint64(at.Unix()))
	msg = append(append(msg, iv...), sealed...)
	mac := hmac.New(sha256.New, k.signing)
	mac.Write(msg)
	return []byte(base64.URLEncoding.EncodeToString(mac.Sum(msg))), nil
}

// Plaintext of a token with no age limit, the HMAC verified in constant time before any decryption
func (k fernetKey) decrypt(token []byte) ([]byte, error) {
	data, err := decodeBase64Lenient(token)
	if err != nil || len(data) < fernetMinToken || data[0] != fernetVersion {
		return nil, errInvalidToken
	}
	body, sum := data[:len(data)-sha256.Size], data[len(data)-sha256.Size:]
	mac := hmac.New(sha256.New, k.signing)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), sum) {
		return nil, errInvalidToken
	}
	iv, sealed := body[9:9+aes.BlockSize], body[9+aes.BlockSize:]
	if len(sealed)%aes.BlockSize != 0 {
		return nil, errInvalidToken
	}
	block, err := aes.NewCipher(k.encryption)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(sealed))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, sealed)
	pad, ok := pkcs7Padding(plain)
	if !ok {
		return nil, errInvalidToken
	}
	return plain[:len(plain)-pad], nil
}

// Length of the PKCS7 padding ending a whole number of blocks, checked without branching on its bytes
func pkcs7Padding(plain []byte) (int, bool) {
	if len(plain) == 0 || len(plain)%aes.BlockSize != 0 {
		return 0, false
	}
	pad := int(plain[len(plain)-1])
	good := subtle.ConstantTimeLessOrEq(1, pad) & subtle.ConstantTimeLessOrEq(pad, aes.BlockSize)
	for i := range aes.BlockSize {
		inPad := subtle.ConstantTimeLessOrEq(i+1, pad)
		same := subtle.ConstantTimeByteEq(plain[len(plain)-1-i], byte(pad))
		good &= subtle.ConstantTimeSelect(inPad, same, 1)
	}
	return pad, good == 1
}

// Bytes of url-safe or standard base64 text, skipping characters outside the alphabet and stopping at complete padding
func decodeBase64Lenient(src []byte) ([]byte, error) {
	out := make([]byte, 0, len(src)*3/4)
	var left byte
	quad, pads := 0, 0
	for _, c := range src {
		if c == '=' {
			if quad >= 2 {
				if pads++; quad+pads >= 4 {
					return out, nil
				}
			}
			continue
		}
		v, ok := base64Digit(c)
		if !ok {
			continue
		}
		pads = 0
		switch quad {
		case 0:
			left, quad = v, 1
		case 1:
			out = append(out, left<<2|v>>4)
			left, quad = v&0x0f, 2
		case 2:
			out = append(out, left<<4|v>>2)
			left, quad = v&0x03, 3
		default:
			out = append(out, left<<6|v)
			left, quad = 0, 0
		}
	}
	if quad != 0 {
		return nil, errBase64
	}
	return out, nil
}

// Six bit value of a base64 character of either alphabet
func base64Digit(c byte) (byte, bool) {
	switch {
	case c >= 'A' && c <= 'Z':
		return c - 'A', true
	case c >= 'a' && c <= 'z':
		return c - 'a' + 26, true
	case c >= '0' && c <= '9':
		return c - '0' + 52, true
	case c == '+' || c == '-':
		return 62, true
	case c == '/' || c == '_':
		return 63, true
	}
	return 0, false
}
