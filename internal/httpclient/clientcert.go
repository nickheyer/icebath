package httpclient

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"os"
	"strings"
	"unicode/utf16"

	"golang.org/x/crypto/scrypt"
)

// Returned when an encrypted private key cannot be opened with the passphrase given
var ErrKeyPassphrase = errors.New("private key passphrase is missing or wrong")

// Client certificate chain and key from PEM files, the key taken from the certificate file when keyFile is empty
func LoadClientCertificate(certFile, keyFile, passphrase string) (tls.Certificate, error) {
	certRaw, err := os.ReadFile(expandHome(certFile))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("client certificate: %w", err)
	}
	keyRaw, keySource := certRaw, certFile
	if keyFile != "" {
		if keyRaw, err = os.ReadFile(expandHome(keyFile)); err != nil {
			return tls.Certificate{}, fmt.Errorf("client key: %w", err)
		}
		keySource = keyFile
	}
	var chain []byte
	var keyBlock *pem.Block
	for rest := certRaw; ; {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = next
		if block.Type == "CERTIFICATE" {
			chain = append(chain, pem.EncodeToMemory(block)...)
		}
	}
	if len(chain) == 0 {
		return tls.Certificate{}, fmt.Errorf("client certificate %s holds no CERTIFICATE block", certFile)
	}
	for rest := keyRaw; keyBlock == nil; {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = next
		if strings.HasSuffix(block.Type, "PRIVATE KEY") {
			keyBlock = block
		}
	}
	if keyBlock == nil {
		return tls.Certificate{}, fmt.Errorf("client key %s holds no PRIVATE KEY block", keySource)
	}
	key, err := privateKey(keyBlock, passphrase)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("client key %s: %w", keySource, err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("client key %s: %w", keySource, err)
	}
	pair, err := tls.X509KeyPair(chain, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("client certificate %s: %w", certFile, err)
	}
	return pair, nil
}

// Private key of a PEM block in any supported form, decrypting RFC 1423 and PKCS 8 encryption with the passphrase
func privateKey(block *pem.Block, passphrase string) (any, error) {
	der := block.Bytes
	encrypted := block.Headers["Proc-Type"] == "4,ENCRYPTED" || block.Type == "ENCRYPTED PRIVATE KEY"
	if encrypted && passphrase == "" {
		return nil, ErrKeyPassphrase
	}
	if block.Headers["Proc-Type"] == "4,ENCRYPTED" {
		plain, err := decryptLegacyPEM(block, passphrase)
		if err != nil {
			return nil, err
		}
		der = plain
	}
	var key any
	var err error
	switch block.Type {
	case "ENCRYPTED PRIVATE KEY":
		plain, derr := decryptPKCS8(der, passphrase)
		if derr != nil {
			return nil, derr
		}
		key, err = x509.ParsePKCS8PrivateKey(plain)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(der)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(der)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(der)
	default:
		return nil, fmt.Errorf("unsupported key block %q", block.Type)
	}
	if err != nil && encrypted {
		return nil, ErrKeyPassphrase
	}
	return key, err
}

// Ciphers RFC 1423 headers may name, the ones OpenSSL 3 opens without its legacy provider
var legacyPEMCiphers = map[string]struct {
	keyLen int
	block  func([]byte) (cipher.Block, error)
}{
	"DES-EDE3-CBC": {24, des.NewTripleDESCipher},
	"AES-128-CBC":  {16, aes.NewCipher},
	"AES-192-CBC":  {24, aes.NewCipher},
	"AES-256-CBC":  {32, aes.NewCipher},
}

// Plain DER of an RFC 1423 encrypted block, the key derived from the passphrase and IV salt with MD5
func decryptLegacyPEM(block *pem.Block, passphrase string) ([]byte, error) {
	name, ivHex, ok := strings.Cut(block.Headers["DEK-Info"], ",")
	spec, known := legacyPEMCiphers[strings.ToUpper(strings.TrimSpace(name))]
	if !ok || !known {
		return nil, fmt.Errorf("unsupported PEM encryption %q", block.Headers["DEK-Info"])
	}
	iv, err := hex.DecodeString(strings.TrimSpace(ivHex))
	if err != nil || len(iv) < 8 {
		return nil, errors.New("PEM encryption header has an invalid IV")
	}
	var key, prev []byte
	for len(key) < spec.keyLen {
		h := md5.New()
		h.Write(prev)
		h.Write([]byte(passphrase))
		h.Write(iv[:8])
		prev = h.Sum(nil)
		key = append(key, prev...)
	}
	c, err := spec.block(key[:spec.keyLen])
	if err != nil {
		return nil, err
	}
	return decryptCBC(c, iv, block.Bytes)
}

var (
	oidPBES2       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidScrypt      = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11591, 4, 11}
	oidPKCS12Des3  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 3}
	oidPKCS12Des2  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 4}
	oidHMACSHA1    = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidDESEDE3CBC  = asn1.ObjectIdentifier{1, 2, 840, 113549, 3, 7}
	oidAES128CBC   = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC   = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC   = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
	hmacByOIDTable = map[string]func() hash.Hash{
		"1.2.840.113549.2.7":  sha1.New,
		"1.2.840.113549.2.8":  sha256.New224,
		"1.2.840.113549.2.9":  sha256.New,
		"1.2.840.113549.2.10": sha512.New384,
		"1.2.840.113549.2.11": sha512.New,
		"1.2.840.113549.2.12": sha512.New512_224,
		"1.2.840.113549.2.13": sha512.New512_256,
	}
)

type encryptedKeyInfo struct {
	Algorithm pkix.AlgorithmIdentifier
	Data      []byte
}

type pbes2Params struct {
	KDF    pkix.AlgorithmIdentifier
	Scheme pkix.AlgorithmIdentifier
}

type pbkdf2Params struct {
	Salt       []byte
	Iterations int
	KeyLength  int                      `asn1:"optional"`
	PRF        pkix.AlgorithmIdentifier `asn1:"optional"`
}

type scryptParams struct {
	Salt      []byte
	Cost      int
	BlockSize int
	Parallel  int
	KeyLength int `asn1:"optional"`
}

type pbeParams struct {
	Salt       []byte
	Iterations int
}

// Plain PKCS 8 DER of an EncryptedPrivateKeyInfo under PBES2 or the PKCS 12 triple DES schemes
func decryptPKCS8(der []byte, passphrase string) ([]byte, error) {
	var info encryptedKeyInfo
	if _, err := asn1.Unmarshal(der, &info); err != nil {
		return nil, fmt.Errorf("parsing encrypted key: %w", err)
	}
	alg := info.Algorithm.Algorithm
	switch {
	case alg.Equal(oidPBES2):
		return decryptPBES2(info.Algorithm.Parameters.FullBytes, info.Data, passphrase)
	case alg.Equal(oidPKCS12Des3), alg.Equal(oidPKCS12Des2):
		var params pbeParams
		if _, err := asn1.Unmarshal(info.Algorithm.Parameters.FullBytes, &params); err != nil {
			return nil, fmt.Errorf("parsing PKCS 12 parameters: %w", err)
		}
		password := bmpPassword(passphrase)
		keyLen := 24
		if alg.Equal(oidPKCS12Des2) {
			keyLen = 16
		}
		key := pkcs12KDF(password, params.Salt, 1, params.Iterations, keyLen)
		iv := pkcs12KDF(password, params.Salt, 2, params.Iterations, 8)
		if keyLen == 16 {
			key = append(key, key[:8]...)
		}
		c, err := des.NewTripleDESCipher(key)
		if err != nil {
			return nil, err
		}
		return decryptCBC(c, iv, info.Data)
	}
	return nil, fmt.Errorf("unsupported key encryption %s", alg)
}

// Plain bytes of a PBES2 ciphertext with a PBKDF2 or scrypt derived AES or triple DES key
func decryptPBES2(rawParams, data []byte, passphrase string) ([]byte, error) {
	var params pbes2Params
	if _, err := asn1.Unmarshal(rawParams, &params); err != nil {
		return nil, fmt.Errorf("parsing PBES2 parameters: %w", err)
	}
	var keyLen int
	var newBlock func([]byte) (cipher.Block, error)
	scheme := params.Scheme.Algorithm
	switch {
	case scheme.Equal(oidAES128CBC):
		keyLen, newBlock = 16, aes.NewCipher
	case scheme.Equal(oidAES192CBC):
		keyLen, newBlock = 24, aes.NewCipher
	case scheme.Equal(oidAES256CBC):
		keyLen, newBlock = 32, aes.NewCipher
	case scheme.Equal(oidDESEDE3CBC):
		keyLen, newBlock = 24, des.NewTripleDESCipher
	default:
		return nil, fmt.Errorf("unsupported PBES2 cipher %s", scheme)
	}
	var iv []byte
	if _, err := asn1.Unmarshal(params.Scheme.Parameters.FullBytes, &iv); err != nil {
		return nil, fmt.Errorf("parsing PBES2 IV: %w", err)
	}
	var key []byte
	kdf := params.KDF.Algorithm
	switch {
	case kdf.Equal(oidPBKDF2):
		var p pbkdf2Params
		if _, err := asn1.Unmarshal(params.KDF.Parameters.FullBytes, &p); err != nil {
			return nil, fmt.Errorf("parsing PBKDF2 parameters: %w", err)
		}
		if p.KeyLength > 0 && p.KeyLength != keyLen {
			return nil, fmt.Errorf("PBKDF2 key length %d does not fit the cipher", p.KeyLength)
		}
		prf := p.PRF.Algorithm
		if len(prf) == 0 {
			prf = oidHMACSHA1
		}
		h, ok := hmacByOIDTable[prf.String()]
		if !ok {
			return nil, fmt.Errorf("unsupported PBKDF2 hash %s", prf)
		}
		derived, err := pbkdf2.Key(h, passphrase, p.Salt, p.Iterations, keyLen)
		if err != nil {
			return nil, err
		}
		key = derived
	case kdf.Equal(oidScrypt):
		var p scryptParams
		if _, err := asn1.Unmarshal(params.KDF.Parameters.FullBytes, &p); err != nil {
			return nil, fmt.Errorf("parsing scrypt parameters: %w", err)
		}
		derived, err := scrypt.Key([]byte(passphrase), p.Salt, p.Cost, p.BlockSize, p.Parallel, keyLen)
		if err != nil {
			return nil, err
		}
		key = derived
	default:
		return nil, fmt.Errorf("unsupported PBES2 key derivation %s", kdf)
	}
	c, err := newBlock(key)
	if err != nil {
		return nil, err
	}
	return decryptCBC(c, iv, data)
}

// CBC decryption with PKCS 7 padding removed, bad padding reported as a wrong passphrase
func decryptCBC(c cipher.Block, iv, data []byte) ([]byte, error) {
	size := c.BlockSize()
	if len(iv) < size || len(data) == 0 || len(data)%size != 0 {
		return nil, ErrKeyPassphrase
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(c, iv[:size]).CryptBlocks(plain, data)
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > size || !bytes.Equal(plain[len(plain)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, ErrKeyPassphrase
	}
	return plain[:len(plain)-pad], nil
}

// Passphrase as a NUL terminated big endian UTF-16 string, the form the PKCS 12 derivation hashes
func bmpPassword(passphrase string) []byte {
	units := utf16.Encode([]rune(passphrase))
	out := make([]byte, 0, 2*len(units)+2)
	for _, u := range units {
		out = append(out, byte(u>>8), byte(u))
	}
	return append(out, 0, 0)
}

// Key material of the PKCS 12 derivation with SHA-1 for a purpose id, one for keys and two for IVs
func pkcs12KDF(password, salt []byte, id byte, iterations, size int) []byte {
	const u, v = sha1.Size, 64
	fill := func(src []byte) []byte {
		if len(src) == 0 {
			return nil
		}
		out := make([]byte, v*((len(src)+v-1)/v))
		for i := range out {
			out[i] = src[i%len(src)]
		}
		return out
	}
	diversifier := bytes.Repeat([]byte{id}, v)
	input := append(fill(salt), fill(password)...)
	var derived []byte
	for len(derived) < size {
		h := sha1.New()
		h.Write(diversifier)
		h.Write(input)
		block := h.Sum(nil)
		for i := 1; i < iterations; i++ {
			sum := sha1.Sum(block)
			block = sum[:]
		}
		derived = append(derived, block...)
		if len(derived) >= size {
			break
		}
		b := make([]byte, v)
		for i := range b {
			b[i] = block[i%u]
		}
		for j := 0; j < len(input); j += v {
			carry := 1
			for k := v - 1; k >= 0; k-- {
				sum := int(input[j+k]) + int(b[k]) + carry
				input[j+k] = byte(sum)
				carry = sum >> 8
			}
		}
	}
	return derived[:size]
}
