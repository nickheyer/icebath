package callback

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"
)

const pkcs7BlockSize = 32

// Plaintext without its 32 byte block PKCS7 padding
func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty decrypted payload")
	}
	pad := int(data[len(data)-1])
	if pad < 1 || pad > pkcs7BlockSize || pad > len(data) {
		return nil, errors.New("invalid PKCS7 padding")
	}
	if !bytes.Equal(data[len(data)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("malformed PKCS7 padding")
	}
	return data[:len(data)-pad], nil
}

// Hex SHA1 of the sorted token, timestamp, nonce and ciphertext
func signature(token, timestamp, nonce, encrypt string) string {
	parts := []string{token, timestamp, nonce, encrypt}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

// Callback message crypto wire compatible with the official WXBizMsgCrypt SDK
type msgCrypt struct {
	token     string
	receiveID string
	key       []byte
}

// Crypto for one app, refusing a missing token, receive id or a malformed 43 character AES key
func newMsgCrypt(token, encodingAESKey, receiveID string) (*msgCrypt, error) {
	switch {
	case token == "":
		return nil, errors.New("token is required")
	case encodingAESKey == "":
		return nil, errors.New("encoding_aes_key is required")
	case utf8.RuneCountInString(encodingAESKey) != 43:
		return nil, errors.New("encoding_aes_key must be 43 chars")
	case receiveID == "":
		return nil, errors.New("receive_id is required")
	}
	key, err := base64.StdEncoding.DecodeString(encodingAESKey + "=")
	if err != nil {
		return nil, errors.New("encoding_aes_key is not valid base64: " + err.Error())
	}
	return &msgCrypt{token: token, receiveID: receiveID, key: key}, nil
}

// Decrypted echostr of a URL verification handshake
func (c *msgCrypt) verifyURL(msgSignature, timestamp, nonce, echostr string) (string, error) {
	raw, err := c.decrypt(msgSignature, timestamp, nonce, echostr)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", errors.New("echostr is not UTF-8")
	}
	return string(raw), nil
}

// XML of a signed AES-CBC payload after the random prefix, length header and receive id check
func (c *msgCrypt) decrypt(msgSignature, timestamp, nonce, encrypt string) ([]byte, error) {
	if signature(c.token, timestamp, nonce, encrypt) != msgSignature {
		return nil, errors.New("signature mismatch")
	}
	cipherText, err := base64.StdEncoding.DecodeString(encrypt)
	if err != nil {
		return nil, errors.New("invalid base64 payload: " + err.Error())
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, errors.New("decrypt failed: " + err.Error())
	}
	if len(cipherText) == 0 || len(cipherText)%aes.BlockSize != 0 {
		return nil, errors.New("decrypt failed: ciphertext is not a whole number of AES blocks")
	}
	plain := make([]byte, len(cipherText))
	cipher.NewCBCDecrypter(block, c.key[:aes.BlockSize]).CryptBlocks(plain, cipherText)
	unpadded, err := pkcs7Unpad(plain)
	if err != nil {
		return nil, err
	}
	if len(unpadded) < 20 {
		return nil, errors.New("decrypt failed: payload shorter than its header")
	}
	content := unpadded[16:]
	end := 4 + int(binary.BigEndian.Uint32(content[:4]))
	if end > len(content) {
		return nil, errors.New("decrypt failed: declared length exceeds the payload")
	}
	receiveID := content[end:]
	if !utf8.Valid(receiveID) {
		return nil, errors.New("decrypt failed: receive id is not UTF-8")
	}
	if string(receiveID) != c.receiveID {
		return nil, errors.New("receive_id mismatch")
	}
	return content[4:end], nil
}
