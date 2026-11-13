package qqbot

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Random base64 256 bit AES key the portal encrypts the bot secret under
func generateBindKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// Opens a base64 nonce, ciphertext and tag sealed with AES-GCM under the base64 key
func decryptSecret(encryptedBase64, keyBase64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encryptedBase64)
	if err != nil {
		return "", fmt.Errorf("decode encrypted secret: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return "", fmt.Errorf("decode bind key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize()+gcm.Overhead() {
		return "", errors.New("encrypted secret is too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("encrypted secret failed authentication")
	}
	if !utf8.Valid(plain) {
		return "", errors.New("decrypted secret is not valid UTF-8")
	}
	return string(plain), nil
}
