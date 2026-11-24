package qqbot

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// ErrInvalidTag is the error of an encrypted secret whose GCM authentication fails
var ErrInvalidTag = errors.New("encrypted secret failed GCM authentication")

// GenerateBindKey returns a random base64 256 bit AES key for create_bind_task
func GenerateBindKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// DecryptSecret opens base64 IV(12) ciphertext tag(16) under the base64 AES key and decodes UTF-8
func DecryptSecret(encryptedBase64, keyBase64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encryptedBase64)
	if err != nil {
		return "", err
	}
	key, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return "", err
	}
	if n := len(key); n != 16 && n != 24 && n != 32 {
		return "", fmt.Errorf("AES key must be 128, 192 or 256 bits, got %d", len(key)*8)
	}
	nonce := raw[:min(12, len(raw))]
	if len(nonce) < 8 {
		return "", errors.New("encrypted secret is too short to hold its nonce")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(nonce))
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, nonce, raw[len(nonce):], nil)
	if err != nil {
		return "", ErrInvalidTag
	}
	return textutil.DecodeUTF8(plain)
}
