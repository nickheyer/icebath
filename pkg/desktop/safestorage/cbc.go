//go:build !windows

package safestorage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"errors"
)

const cbcSalt = "saltysalt"

var cbcIV = bytes.Repeat([]byte{' '}, aes.BlockSize)

// AES-128 key Chromium derives from a key store password with PBKDF2 SHA1 over the fixed salt
func deriveCBCKey(password string, iterations int) ([]byte, error) {
	return pbkdf2.Key(sha1.New, password, []byte(cbcSalt), iterations, 16)
}

// AES-128-CBC with PKCS7 padding and the space filled IV behind a version prefix
func cbcSeal(key []byte, prefix string, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	data := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	cipher.NewCBCEncrypter(block, cbcIV).CryptBlocks(data, data)
	return append([]byte(prefix), data...), nil
}

// Reverses cbcSeal on the bytes after the version prefix, refusing bad padding
func cbcOpen(key []byte, data []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("ciphertext is not a whole number of blocks")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := append([]byte{}, data...)
	cipher.NewCBCDecrypter(block, cbcIV).CryptBlocks(out, out)
	pad := int(out[len(out)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(out) || !bytes.Equal(out[len(out)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("bad padding")
	}
	return out[:len(out)-pad], nil
}
