package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"
)

// Fernet, the token format the vault file is kept in: AES-128-CBC with an HMAC-SHA256 over a version, a time stamp and the IV
const (
	fernetVersion = 0x80
	fernetHeader  = 1 + 8 + aes.BlockSize
	fernetMinimum = fernetHeader + aes.BlockSize + sha256.Size
	fernetKeySize = 32
)

var errFernet = errors.New("invalid fernet token")

// Encrypter and decrypter over one key
type fernet struct {
	signing []byte
	block   cipher.Block
}

// New key of 32 random bytes in padded url-safe base64
func fernetKey() []byte {
	raw := make([]byte, fernetKeySize)
	rand.Read(raw)
	return []byte(base64.URLEncoding.EncodeToString(raw))
}

// Fernet over a key in padded url-safe base64
func newFernet(key []byte) (*fernet, error) {
	raw, err := base64.URLEncoding.DecodeString(string(key))
	if err != nil || len(raw) != fernetKeySize {
		return nil, errors.New("fernet key must be 32 bytes of padded url-safe base64")
	}
	block, err := aes.NewCipher(raw[16:])
	if err != nil {
		return nil, err
	}
	return &fernet{signing: raw[:16:16], block: block}, nil
}

// Token of the data, stamped now under a random IV
func (f *fernet) encrypt(data []byte) []byte {
	iv := make([]byte, aes.BlockSize)
	rand.Read(iv)
	pad := aes.BlockSize - len(data)%aes.BlockSize
	msg := make([]byte, fernetHeader+len(data)+pad, fernetHeader+len(data)+pad+sha256.Size)
	msg[0] = fernetVersion
	binary.BigEndian.PutUint64(msg[1:9], uint64(time.Now().Unix()))
	copy(msg[9:fernetHeader], iv)
	body := msg[fernetHeader:]
	copy(body, data)
	for i := len(data); i < len(body); i++ {
		body[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(f.block, iv).CryptBlocks(body, body)
	mac := hmac.New(sha256.New, f.signing)
	mac.Write(msg)
	return []byte(base64.URLEncoding.EncodeToString(mac.Sum(msg)))
}

// Data of a token whatever its age, an error for every token that does not verify and decrypt
func (f *fernet) decrypt(token []byte) ([]byte, error) {
	msg, err := base64.URLEncoding.DecodeString(string(token))
	n := len(msg)
	if err != nil || n < fernetMinimum || msg[0] != fernetVersion || (n-fernetHeader-sha256.Size)%aes.BlockSize != 0 {
		return nil, errFernet
	}
	mac := hmac.New(sha256.New, f.signing)
	mac.Write(msg[:n-sha256.Size])
	if !hmac.Equal(mac.Sum(nil), msg[n-sha256.Size:]) {
		return nil, errFernet
	}
	body := msg[fernetHeader : n-sha256.Size]
	cipher.NewCBCDecrypter(f.block, msg[9:fernetHeader]).CryptBlocks(body, body)
	pad := int(body[len(body)-1])
	if pad == 0 || pad > aes.BlockSize {
		return nil, errFernet
	}
	for _, b := range body[len(body)-pad:] {
		if int(b) != pad {
			return nil, errFernet
		}
	}
	return body[:len(body)-pad], nil
}
