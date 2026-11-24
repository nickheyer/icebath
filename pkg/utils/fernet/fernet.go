package fernetutil

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Error of every token that is malformed, forged, signed with another key or badly padded
var ErrInvalidToken = errors.New("invalid fernet token")

// Error of a token older than the time to live it was checked against, also matching ErrInvalidToken
var ErrExpiredToken = fmt.Errorf("%w: expired", ErrInvalidToken)

// Error of a key that is not 32 bytes in padded url-safe base64
var ErrInvalidKey = errors.New("fernet key must be 32 bytes of padded url-safe base64")

const (
	version     = 0x80
	headerSize  = 1 + 8 + aes.BlockSize
	macSize     = sha256.Size
	minTokenLen = headerSize + aes.BlockSize + macSize
	keySize     = 32
	clockSkew   = 60 * time.Second
)

// Token encrypter and decrypter over one key, safe for concurrent use
type Fernet struct {
	signing []byte
	block   cipher.Block
}

// New key of 32 random bytes in padded url-safe base64
func GenerateKey() []byte {
	raw := make([]byte, keySize)
	rand.Read(raw)
	return base64.URLEncoding.AppendEncode(make([]byte, 0, base64.URLEncoding.EncodedLen(keySize)), raw)
}

// Fernet over a key in padded url-safe base64, ErrInvalidKey for anything else
func New(key []byte) (*Fernet, error) {
	raw := make([]byte, base64.URLEncoding.DecodedLen(len(key)))
	n, err := base64.URLEncoding.Decode(raw, key)
	if err != nil || n != keySize {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(raw[16:keySize])
	if err != nil {
		return nil, ErrInvalidKey
	}
	return &Fernet{signing: raw[:16:16], block: block}, nil
}

// Token of data stamped with the current time under a random IV
func (f *Fernet) Encrypt(data []byte) []byte {
	var iv [aes.BlockSize]byte
	rand.Read(iv[:])
	return f.seal(data, time.Now().Unix(), iv[:])
}

// Token of data stamped with the given Unix time under the given IV
func (f *Fernet) seal(data []byte, at int64, iv []byte) []byte {
	pad := aes.BlockSize - len(data)%aes.BlockSize
	msg := make([]byte, headerSize+len(data)+pad, headerSize+len(data)+pad+macSize)
	msg[0] = version
	binary.BigEndian.PutUint64(msg[1:9], uint64(at))
	copy(msg[9:headerSize], iv)
	body := msg[headerSize:]
	copy(body, data)
	for i := len(data); i < len(body); i++ {
		body[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(f.block, iv).CryptBlocks(body, body)
	mac := hmac.New(sha256.New, f.signing)
	mac.Write(msg)
	msg = mac.Sum(msg)
	return base64.URLEncoding.AppendEncode(make([]byte, 0, base64.URLEncoding.EncodedLen(len(msg))), msg)
}

// Plaintext of a token whatever its age, ErrInvalidToken for every token that does not verify and decrypt
func (f *Fernet) Decrypt(token []byte) ([]byte, error) {
	msg, err := f.verify(token)
	if err != nil {
		return nil, err
	}
	return f.open(msg)
}

// Plaintext of a token stamped no longer than ttl ago and not more than a minute ahead of the clock, ErrExpiredToken past the ttl; a ttl that is not positive checks no age
func (f *Fernet) DecryptTTL(token []byte, ttl time.Duration) ([]byte, error) {
	msg, err := f.verify(token)
	if err != nil {
		return nil, err
	}
	if ttl > 0 {
		stamp := binary.BigEndian.Uint64(msg[1:9])
		now := time.Now()
		if stamp > uint64(now.Add(clockSkew).Unix()) {
			return nil, ErrInvalidToken
		}
		if time.Unix(int64(stamp), 0).Add(ttl).Before(now) {
			return nil, ErrExpiredToken
		}
	}
	return f.open(msg)
}

// Decoded token with its version, length and signature checked, the signature compared in constant time
func (f *Fernet) verify(token []byte) ([]byte, error) {
	msg := make([]byte, base64.URLEncoding.DecodedLen(len(token)))
	n, err := base64.URLEncoding.Decode(msg, token)
	if err != nil || n < minTokenLen || msg[0] != version || (n-headerSize-macSize)%aes.BlockSize != 0 {
		return nil, ErrInvalidToken
	}
	msg = msg[:n]
	mac := hmac.New(sha256.New, f.signing)
	mac.Write(msg[:n-macSize])
	if !hmac.Equal(mac.Sum(nil), msg[n-macSize:]) {
		return nil, ErrInvalidToken
	}
	return msg, nil
}

// Plaintext of a verified token, decrypted in place with its padding checked and removed
func (f *Fernet) open(msg []byte) ([]byte, error) {
	body := msg[headerSize : len(msg)-macSize]
	cipher.NewCBCDecrypter(f.block, msg[9:headerSize]).CryptBlocks(body, body)
	pad := int(body[len(body)-1])
	if pad == 0 || pad > aes.BlockSize {
		return nil, ErrInvalidToken
	}
	for _, b := range body[len(body)-pad:] {
		if int(b) != pad {
			return nil, ErrInvalidToken
		}
	}
	return body[:len(body)-pad], nil
}
