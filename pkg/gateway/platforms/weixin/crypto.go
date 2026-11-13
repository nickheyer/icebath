package weixin

import (
	"bytes"
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// PKCS7 padded AES-ECB ciphertext of the plaintext, the CDN media encryption
func encryptECB(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(bytes.Clone(plaintext), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(padded))
	for i := 0; i < len(padded); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], padded[i:i+aes.BlockSize])
	}
	return out, nil
}

// AES-ECB plaintext of the ciphertext with well formed PKCS7 padding removed
func decryptECB(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d is not a multiple of the AES block size", len(ciphertext))
	}
	out := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], ciphertext[i:i+aes.BlockSize])
	}
	if n := len(out); n > 0 {
		pad := int(out[n-1])
		if pad >= 1 && pad <= aes.BlockSize && pad <= n && bytes.Equal(out[n-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
			return out[:n-pad], nil
		}
	}
	return out, nil
}

// Standard base64 decode that ignores characters outside the alphabet
func decodeBase64Lenient(s string) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '+', r == '/', r == '=':
			return r
		}
		return -1
	}, s)
	return base64.StdEncoding.DecodeString(clean)
}

// Sixteen byte key from raw key bytes or from thirty two hex characters
func keyFromBytes(decoded []byte) ([]byte, error) {
	if len(decoded) == 16 {
		return decoded, nil
	}
	if len(decoded) == 32 {
		if key, err := hex.DecodeString(string(decoded)); err == nil {
			return key, nil
		}
	}
	return nil, fmt.Errorf("unexpected aes_key format (%d decoded bytes)", len(decoded))
}

// Media key of a base64 aes_key field, either raw bytes or a hex string inside the base64
func parseAESKey(b64 string) ([]byte, error) {
	decoded, err := decodeBase64Lenient(b64)
	if err != nil {
		return nil, fmt.Errorf("aes_key is not valid base64: %w", err)
	}
	return keyFromBytes(decoded)
}

// Media key of the raw hex aeskey an image item may carry beside its media block
func parseHexAESKey(hexKey string) ([]byte, error) {
	raw, err := hex.DecodeString(strings.Join(strings.Fields(hexKey), ""))
	if err != nil {
		return nil, fmt.Errorf("image aeskey is not valid hex: %w", err)
	}
	return keyFromBytes(raw)
}
