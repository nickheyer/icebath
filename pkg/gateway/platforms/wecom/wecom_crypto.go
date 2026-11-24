package wecom

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// WeComCryptoError is implemented by every callback crypto failure
type WeComCryptoError interface {
	error
	weComCryptoError()
}

type SignatureError struct{ Msg string }
type DecryptError struct{ Msg string }

func (e *SignatureError) Error() string   { return e.Msg }
func (e *DecryptError) Error() string     { return e.Msg }
func (*SignatureError) weComCryptoError() {}
func (*DecryptError) weComCryptoError()   {}

const pkcs7BlockSize = 32

// PKCS7Decode strips and validates the 32 byte block padding
func PKCS7Decode(decrypted []byte) ([]byte, error) {
	if len(decrypted) == 0 {
		return nil, &DecryptError{"empty decrypted payload"}
	}
	pad := int(decrypted[len(decrypted)-1])
	if pad < 1 || pad > pkcs7BlockSize {
		return nil, &DecryptError{"invalid PKCS7 padding"}
	}
	tail := decrypted[max(0, len(decrypted)-pad):]
	if !bytes.Equal(tail, bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, &DecryptError{"malformed PKCS7 padding"}
	}
	return decrypted[:max(0, len(decrypted)-pad)], nil
}

func sha1Signature(token, timestamp, nonce, encrypt string) string {
	parts := []string{token, timestamp, nonce, encrypt}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

// WXBizMsgCrypt is the callback crypto helper wire compatible with the official BizMsgCrypt SDK
type WXBizMsgCrypt struct {
	Token     string
	ReceiveID string
	Key       []byte
	IV        []byte
}

// NewWXBizMsgCrypt validates the token, the 43 char AES key and the receive id
func NewWXBizMsgCrypt(token, encodingAESKey, receiveID string) (*WXBizMsgCrypt, error) {
	for _, c := range []struct {
		bad bool
		msg string
	}{
		{token == "", "token is required"}, {encodingAESKey == "", "encoding_aes_key is required"},
		{utf8.RuneCountInString(encodingAESKey) != 43, "encoding_aes_key must be 43 chars"}, {receiveID == "", "receive_id is required"},
	} {
		if c.bad {
			return nil, fmt.Errorf("%s", c.msg)
		}
	}
	key, err := base64.StdEncoding.DecodeString(encodingAESKey + "=")
	if err != nil {
		return nil, err
	}
	return &WXBizMsgCrypt{Token: token, ReceiveID: receiveID, Key: key, IV: key[:min(16, len(key))]}, nil
}

func (c *WXBizMsgCrypt) block() (cipher.Block, error) {
	block, err := aes.NewCipher(c.Key)
	if err != nil {
		return nil, fmt.Errorf("Invalid key size (%d) for AES.", len(c.Key)*8)
	}
	if len(c.IV) != aes.BlockSize {
		return nil, fmt.Errorf("Invalid IV size (%d) for CBC.", len(c.IV))
	}
	return block, nil
}

// VerifyURL decrypts the echostr of a URL verification and decodes it as UTF-8
func (c *WXBizMsgCrypt) VerifyURL(msgSignature, timestamp, nonce, echostr string) (string, error) {
	raw, err := c.Decrypt(msgSignature, timestamp, nonce, echostr)
	if err != nil {
		return "", err
	}
	return textutil.DecodeUTF8(raw)
}

// Decrypt checks the signature, opens the AES-CBC payload and returns the XML after the receive id check
func (c *WXBizMsgCrypt) Decrypt(msgSignature, timestamp, nonce, encrypt string) ([]byte, error) {
	if sha1Signature(c.Token, timestamp, nonce, encrypt) != msgSignature {
		return nil, &SignatureError{"signature mismatch"}
	}
	cipherText, err := base64.StdEncoding.DecodeString(encrypt)
	if err != nil {
		return nil, &DecryptError{fmt.Sprintf("invalid base64 payload: %v", err)}
	}
	xmlContent, receiveID, err := c.open(cipherText)
	if err != nil {
		if _, ok := err.(WeComCryptoError); ok {
			return nil, err
		}
		return nil, &DecryptError{fmt.Sprintf("decrypt failed: %v", err)}
	}
	if receiveID != c.ReceiveID {
		return nil, &DecryptError{"receive_id mismatch"}
	}
	return xmlContent, nil
}

// Decrypts, unpads, skips the random prefix and splits the length prefixed XML from the receive id
func (c *WXBizMsgCrypt) open(cipherText []byte) ([]byte, string, error) {
	block, err := c.block()
	if err != nil {
		return nil, "", err
	}
	if len(cipherText)%aes.BlockSize != 0 {
		return nil, "", fmt.Errorf("The length of the provided data is not a multiple of the block length.")
	}
	plain := make([]byte, len(cipherText))
	cipher.NewCBCDecrypter(block, c.IV).CryptBlocks(plain, cipherText)
	unpadded, err := PKCS7Decode(plain)
	if err != nil {
		return nil, "", err
	}
	content := unpadded[min(16, len(unpadded)):]
	if len(content) < 4 {
		return nil, "", fmt.Errorf("unpack requires a buffer of 4 bytes")
	}
	xmlLength := int(binary.BigEndian.Uint32(content[:4]))
	end := min(4+xmlLength, len(content))
	receiveID, err := textutil.DecodeUTF8(content[end:])
	if err != nil {
		return nil, "", err
	}
	return content[4:end], receiveID, nil
}
