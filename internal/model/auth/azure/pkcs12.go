package azure

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/hmac"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"hash"
	"strings"
	"unicode/utf16"
)

var (
	oidData           = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidEncryptedData  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 6}
	oidKeyBag         = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 1}
	oidShroudedKeyBag = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 2}
	oidCertBag        = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 3}
	oidX509Cert       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 22, 1}
	oidSHA3DES        = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 3}
	oidSHA2DES        = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 4}
	oidSHA128RC2      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 5}
	oidSHA40RC2       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 6}
	oidPBES2          = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2         = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidDESEDE3CBC     = asn1.ObjectIdentifier{1, 2, 840, 113549, 3, 7}
	aesCBCKeySizes    = map[string]int{"2.16.840.1.101.3.4.1.2": 16, "2.16.840.1.101.3.4.1.22": 24, "2.16.840.1.101.3.4.1.42": 32}
	hmacDigests       = map[string]func() hash.Hash{
		"1.2.840.113549.2.7": sha1.New, "1.2.840.113549.2.8": sha256.New224, "1.2.840.113549.2.9": sha256.New,
		"1.2.840.113549.2.10": sha512.New384, "1.2.840.113549.2.11": sha512.New,
	}
	macDigests = map[string]func() hash.Hash{
		"1.3.14.3.2.26": sha1.New, "2.16.840.1.101.3.4.2.4": sha256.New224, "2.16.840.1.101.3.4.2.1": sha256.New,
		"2.16.840.1.101.3.4.2.2": sha512.New384, "2.16.840.1.101.3.4.2.3": sha512.New,
	}
	errPKCS12   = errors.New("malformed PKCS12 archive")
	errPassword = errors.New("invalid password or encrypted data")
)

type pfxArchive struct {
	Version  int
	AuthSafe contentInfo
	MacData  macData `asn1:"optional"`
}

type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"tag:0,explicit,optional"`
}

type macData struct {
	Mac        digestInfo
	MacSalt    []byte
	Iterations int `asn1:"optional,default:1"`
}

type digestInfo struct {
	Algorithm pkix.AlgorithmIdentifier
	Digest    []byte
}

type encryptedData struct {
	Version int
	Info    encryptedContentInfo
}

type encryptedContentInfo struct {
	ContentType asn1.ObjectIdentifier
	Algorithm   pkix.AlgorithmIdentifier
	Content     []byte `asn1:"tag:0,optional"`
}

type safeBag struct {
	ID         asn1.ObjectIdentifier
	Value      asn1.RawValue `asn1:"tag:0,explicit"`
	Attributes asn1.RawValue `asn1:"optional"`
}

type certBag struct {
	ID   asn1.ObjectIdentifier
	Data []byte `asn1:"tag:0,explicit"`
}

type encryptedKeyInfo struct {
	Algorithm pkix.AlgorithmIdentifier
	Data      []byte
}

type pbeParams struct {
	Salt       []byte
	Iterations int
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

func derElement(tag, content []byte) []byte {
	out := append([]byte{}, tag...)
	if n := len(content); n < 0x80 {
		out = append(out, byte(n))
	} else {
		var size []byte
		for v := n; v > 0; v >>= 8 {
			size = append([]byte{byte(v)}, size...)
		}
		out = append(append(out, 0x80|byte(len(size))), size...)
	}
	return append(out, content...)
}

func berElement(in []byte, depth int) ([]byte, []byte, error) {
	if len(in) < 2 || depth > 32 {
		return nil, nil, errPKCS12
	}
	tagLen := 1
	if in[0]&0x1f == 0x1f {
		for tagLen < len(in) && in[tagLen]&0x80 != 0 {
			tagLen++
		}
		tagLen++
	}
	if tagLen >= len(in) {
		return nil, nil, errPKCS12
	}
	tag := append([]byte{}, in[:tagLen]...)
	constructed := in[0]&0x20 != 0
	rest := in[tagLen:]
	first := rest[0]
	rest = rest[1:]
	var children [][]byte
	if first == 0x80 {
		if !constructed {
			return nil, nil, errPKCS12
		}
		for {
			if len(rest) >= 2 && rest[0] == 0 && rest[1] == 0 {
				rest = rest[2:]
				break
			}
			child, remaining, err := berElement(rest, depth+1)
			if err != nil {
				return nil, nil, err
			}
			children, rest = append(children, child), remaining
		}
	} else {
		length := int(first)
		if first&0x80 != 0 {
			n := int(first & 0x7f)
			if n > 4 || n > len(rest) {
				return nil, nil, errPKCS12
			}
			length = 0
			for _, b := range rest[:n] {
				length = length<<8 | int(b)
			}
			rest = rest[n:]
		}
		if length < 0 || length > len(rest) {
			return nil, nil, errPKCS12
		}
		content := rest[:length]
		rest = rest[length:]
		if !constructed {
			return derElement(tag, content), rest, nil
		}
		for len(content) > 0 {
			child, remaining, err := berElement(content, depth+1)
			if err != nil {
				return nil, nil, err
			}
			children, content = append(children, child), remaining
		}
	}
	if tag[0] == 0x24 {
		var octets []byte
		for _, child := range children {
			var raw asn1.RawValue
			if _, err := asn1.Unmarshal(child, &raw); err != nil {
				return nil, nil, errPKCS12
			}
			octets = append(octets, raw.Bytes...)
		}
		return derElement([]byte{0x04}, octets), rest, nil
	}
	return derElement(tag, bytes.Join(children, nil)), rest, nil
}

func pkcs12Derive(newHash func() hash.Hash, salt, password []byte, iterations int, id byte, size int) []byte {
	h := newHash()
	v := h.BlockSize()
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
		h.Reset()
		h.Write(diversifier)
		h.Write(input)
		block := h.Sum(nil)
		for i := 1; i < iterations; i++ {
			h.Reset()
			h.Write(block)
			block = h.Sum(nil)
		}
		derived = append(derived, block...)
		if len(derived) >= size {
			break
		}
		adjust := fill(block)[:v]
		for j := 0; j < len(input); j += v {
			carry := 1
			for k := v - 1; k >= 0; k-- {
				sum := int(input[j+k]) + int(adjust[k]) + carry
				input[j+k], carry = byte(sum), sum>>8
			}
		}
	}
	return derived[:size]
}

func bmpPassword(password string) []byte {
	units := utf16.Encode([]rune(password))
	out := make([]byte, 0, len(units)*2+2)
	for _, u := range units {
		out = append(out, byte(u>>8), byte(u))
	}
	return append(out, 0, 0)
}

func cbcDecrypt(block cipher.Block, iv, data []byte) ([]byte, error) {
	size := block.BlockSize()
	if len(data) == 0 || len(data)%size != 0 || len(iv) != size {
		return nil, errPassword
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad == 0 || pad > size || !bytes.Equal(out[len(out)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errPassword
	}
	return out[:len(out)-pad], nil
}

func pbeDecrypt(alg pkix.AlgorithmIdentifier, data []byte, password string) ([]byte, error) {
	var block cipher.Block
	var iv []byte
	var err error
	switch {
	case alg.Algorithm.Equal(oidSHA3DES), alg.Algorithm.Equal(oidSHA2DES), alg.Algorithm.Equal(oidSHA40RC2), alg.Algorithm.Equal(oidSHA128RC2):
		var params pbeParams
		if _, err := asn1.Unmarshal(alg.Parameters.FullBytes, &params); err != nil || params.Iterations <= 0 {
			return nil, errPKCS12
		}
		secret := bmpPassword(password)
		iv = pkcs12Derive(sha1.New, params.Salt, secret, params.Iterations, 2, 8)
		switch {
		case alg.Algorithm.Equal(oidSHA3DES):
			block, err = des.NewTripleDESCipher(pkcs12Derive(sha1.New, params.Salt, secret, params.Iterations, 1, 24))
		case alg.Algorithm.Equal(oidSHA2DES):
			key := pkcs12Derive(sha1.New, params.Salt, secret, params.Iterations, 1, 16)
			block, err = des.NewTripleDESCipher(append(key, key[:8]...))
		case alg.Algorithm.Equal(oidSHA40RC2):
			block = newRC2(pkcs12Derive(sha1.New, params.Salt, secret, params.Iterations, 1, 5), 40)
		default:
			block = newRC2(pkcs12Derive(sha1.New, params.Salt, secret, params.Iterations, 1, 16), 128)
		}
	case alg.Algorithm.Equal(oidPBES2):
		var params pbes2Params
		if _, err := asn1.Unmarshal(alg.Parameters.FullBytes, &params); err != nil || !params.KDF.Algorithm.Equal(oidPBKDF2) {
			return nil, errPKCS12
		}
		var kdf pbkdf2Params
		if _, err := asn1.Unmarshal(params.KDF.Parameters.FullBytes, &kdf); err != nil || kdf.Iterations <= 0 {
			return nil, errPKCS12
		}
		prf := sha1.New
		if len(kdf.PRF.Algorithm) > 0 {
			if prf = hmacDigests[kdf.PRF.Algorithm.String()]; prf == nil {
				return nil, errors.New("unsupported PKCS12 key derivation digest " + kdf.PRF.Algorithm.String())
			}
		}
		keyLen, isAES := aesCBCKeySizes[params.Scheme.Algorithm.String()]
		if !isAES {
			if !params.Scheme.Algorithm.Equal(oidDESEDE3CBC) {
				return nil, errors.New("unsupported PKCS12 cipher " + params.Scheme.Algorithm.String())
			}
			keyLen = 24
		}
		if _, err := asn1.Unmarshal(params.Scheme.Parameters.FullBytes, &iv); err != nil {
			return nil, errPKCS12
		}
		key, err := pbkdf2.Key(prf, password, kdf.Salt, kdf.Iterations, keyLen)
		if err != nil {
			return nil, errPKCS12
		}
		if isAES {
			block, err = aes.NewCipher(key)
		} else {
			block, err = des.NewTripleDESCipher(key)
		}
		if err != nil {
			return nil, errPKCS12
		}
	default:
		return nil, errors.New("unsupported PKCS12 encryption " + alg.Algorithm.String())
	}
	if err != nil {
		return nil, errPKCS12
	}
	return cbcDecrypt(block, iv, data)
}

func decryptShroudedKey(der []byte, password string) ([]byte, error) {
	var info encryptedKeyInfo
	if _, err := asn1.Unmarshal(der, &info); err != nil {
		return nil, errPKCS12
	}
	return pbeDecrypt(info.Algorithm, info.Data, password)
}

func decryptLegacyPEM(block *pem.Block, password string) ([]byte, error) {
	name, ivHex, ok := strings.Cut(block.Headers["DEK-Info"], ",")
	iv, err := hex.DecodeString(ivHex)
	if !ok || err != nil || len(iv) < 8 {
		return nil, errors.New("private key carries an unreadable DEK-Info header")
	}
	keyLen := map[string]int{"DES-CBC": 8, "DES-EDE3-CBC": 24, "AES-128-CBC": 16, "AES-192-CBC": 24, "AES-256-CBC": 32}[name]
	if keyLen == 0 {
		return nil, errors.New("private key is encrypted with the unsupported cipher " + name)
	}
	var key, previous []byte
	for len(key) < keyLen {
		digest := md5.New()
		digest.Write(previous)
		digest.Write([]byte(password))
		digest.Write(iv[:8])
		previous = digest.Sum(nil)
		key = append(key, previous...)
	}
	var cipherBlock cipher.Block
	switch {
	case name == "DES-CBC":
		cipherBlock, err = des.NewCipher(key[:keyLen])
	case name == "DES-EDE3-CBC":
		cipherBlock, err = des.NewTripleDESCipher(key[:keyLen])
	default:
		cipherBlock, err = aes.NewCipher(key[:keyLen])
	}
	if err != nil {
		return nil, errPassword
	}
	return cbcDecrypt(cipherBlock, iv, block.Bytes)
}

func decodePKCS12(raw []byte, password string) (any, []*x509.Certificate, error) {
	der, rest, err := berElement(raw, 0)
	if err != nil || len(rest) != 0 {
		return nil, nil, errPKCS12
	}
	var pfx pfxArchive
	if _, err := asn1.Unmarshal(der, &pfx); err != nil || !pfx.AuthSafe.ContentType.Equal(oidData) {
		return nil, nil, errPKCS12
	}
	var authSafe []byte
	if _, err := asn1.Unmarshal(pfx.AuthSafe.Content.Bytes, &authSafe); err != nil {
		return nil, nil, errPKCS12
	}
	if len(pfx.MacData.Mac.Algorithm.Algorithm) > 0 {
		newHash := macDigests[pfx.MacData.Mac.Algorithm.Algorithm.String()]
		if newHash == nil || pfx.MacData.Iterations <= 0 {
			return nil, nil, errPKCS12
		}
		candidates := [][]byte{bmpPassword(password)}
		if password == "" {
			candidates = append(candidates, nil)
		}
		verified := false
		for _, secret := range candidates {
			mac := hmac.New(newHash, pkcs12Derive(newHash, pfx.MacData.MacSalt, secret, pfx.MacData.Iterations, 3, newHash().Size()))
			mac.Write(authSafe)
			verified = verified || hmac.Equal(mac.Sum(nil), pfx.MacData.Mac.Digest)
		}
		if !verified {
			return nil, nil, errPassword
		}
	}
	var contents []contentInfo
	if _, err := asn1.Unmarshal(authSafe, &contents); err != nil {
		return nil, nil, errPKCS12
	}
	var key any
	var certs []*x509.Certificate
	for _, content := range contents {
		var safe []byte
		switch {
		case content.ContentType.Equal(oidData):
			if _, err := asn1.Unmarshal(content.Content.Bytes, &safe); err != nil {
				return nil, nil, errPKCS12
			}
		case content.ContentType.Equal(oidEncryptedData):
			var sealed encryptedData
			if _, err := asn1.Unmarshal(content.Content.Bytes, &sealed); err != nil {
				return nil, nil, errPKCS12
			}
			if safe, err = pbeDecrypt(sealed.Info.Algorithm, sealed.Info.Content, password); err != nil {
				return nil, nil, err
			}
		default:
			return nil, nil, errPKCS12
		}
		var bags []safeBag
		if _, err := asn1.Unmarshal(safe, &bags); err != nil {
			return nil, nil, errPKCS12
		}
		for _, bag := range bags {
			switch {
			case bag.ID.Equal(oidCertBag):
				var held certBag
				if _, err := asn1.Unmarshal(bag.Value.Bytes, &held); err != nil {
					return nil, nil, errPKCS12
				}
				if !held.ID.Equal(oidX509Cert) {
					continue
				}
				cert, err := x509.ParseCertificate(held.Data)
				if err != nil {
					return nil, nil, errPKCS12
				}
				certs = append(certs, cert)
			case bag.ID.Equal(oidKeyBag), bag.ID.Equal(oidShroudedKeyBag):
				keyDER := bag.Value.Bytes
				if bag.ID.Equal(oidShroudedKeyBag) {
					if keyDER, err = decryptShroudedKey(keyDER, password); err != nil {
						return nil, nil, err
					}
				}
				if key, err = x509.ParsePKCS8PrivateKey(keyDER); err != nil {
					return nil, nil, errPKCS12
				}
			}
		}
	}
	if rsaKey, ok := key.(*rsa.PrivateKey); ok {
		for i, cert := range certs {
			if pub, ok := cert.PublicKey.(*rsa.PublicKey); ok && pub.Equal(&rsaKey.PublicKey) {
				leaf := certs[i]
				copy(certs[1:i+1], certs[:i])
				certs[0] = leaf
				break
			}
		}
	}
	return key, certs, nil
}

var rc2PiTable = [256]byte{
	0xd9, 0x78, 0xf9, 0xc4, 0x19, 0xdd, 0xb5, 0xed, 0x28, 0xe9, 0xfd, 0x79, 0x4a, 0xa0, 0xd8, 0x9d,
	0xc6, 0x7e, 0x37, 0x83, 0x2b, 0x76, 0x53, 0x8e, 0x62, 0x4c, 0x64, 0x88, 0x44, 0x8b, 0xfb, 0xa2,
	0x17, 0x9a, 0x59, 0xf5, 0x87, 0xb3, 0x4f, 0x13, 0x61, 0x45, 0x6d, 0x8d, 0x09, 0x81, 0x7d, 0x32,
	0xbd, 0x8f, 0x40, 0xeb, 0x86, 0xb7, 0x7b, 0x0b, 0xf0, 0x95, 0x21, 0x22, 0x5c, 0x6b, 0x4e, 0x82,
	0x54, 0xd6, 0x65, 0x93, 0xce, 0x60, 0xb2, 0x1c, 0x73, 0x56, 0xc0, 0x14, 0xa7, 0x8c, 0xf1, 0xdc,
	0x12, 0x75, 0xca, 0x1f, 0x3b, 0xbe, 0xe4, 0xd1, 0x42, 0x3d, 0xd4, 0x30, 0xa3, 0x3c, 0xb6, 0x26,
	0x6f, 0xbf, 0x0e, 0xda, 0x46, 0x69, 0x07, 0x57, 0x27, 0xf2, 0x1d, 0x9b, 0xbc, 0x94, 0x43, 0x03,
	0xf8, 0x11, 0xc7, 0xf6, 0x90, 0xef, 0x3e, 0xe7, 0x06, 0xc3, 0xd5, 0x2f, 0xc8, 0x66, 0x1e, 0xd7,
	0x08, 0xe8, 0xea, 0xde, 0x80, 0x52, 0xee, 0xf7, 0x84, 0xaa, 0x72, 0xac, 0x35, 0x4d, 0x6a, 0x2a,
	0x96, 0x1a, 0xd2, 0x71, 0x5a, 0x15, 0x49, 0x74, 0x4b, 0x9f, 0xd0, 0x5e, 0x04, 0x18, 0xa4, 0xec,
	0xc2, 0xe0, 0x41, 0x6e, 0x0f, 0x51, 0xcb, 0xcc, 0x24, 0x91, 0xaf, 0x50, 0xa1, 0xf4, 0x70, 0x39,
	0x99, 0x7c, 0x3a, 0x85, 0x23, 0xb8, 0xb4, 0x7a, 0xfc, 0x02, 0x36, 0x5b, 0x25, 0x55, 0x97, 0x31,
	0x2d, 0x5d, 0xfa, 0x98, 0xe3, 0x8a, 0x92, 0xae, 0x05, 0xdf, 0x29, 0x10, 0x67, 0x6c, 0xba, 0xc9,
	0xd3, 0x00, 0xe6, 0xcf, 0xe1, 0x9e, 0xa8, 0x2c, 0x63, 0x16, 0x01, 0x3f, 0x58, 0xe2, 0x89, 0xa9,
	0x0d, 0x38, 0x34, 0x1b, 0xab, 0x33, 0xff, 0xb0, 0xbb, 0x48, 0x0c, 0x5f, 0xb9, 0xb1, 0xcd, 0x2e,
	0xc5, 0xf3, 0xdb, 0x47, 0xe5, 0xa5, 0x9c, 0x77, 0x0a, 0xa6, 0x20, 0x68, 0xfe, 0x7f, 0xc1, 0xad,
}

type rc2 struct{ k [64]uint16 }

func newRC2(key []byte, effectiveBits int) *rc2 {
	var expanded [128]byte
	copy(expanded[:], key)
	size := len(key)
	for i := size; i < 128; i++ {
		expanded[i] = rc2PiTable[expanded[i-1]+expanded[i-size]]
	}
	effectiveBytes := (effectiveBits + 7) / 8
	mask := byte(255 % (1 << uint(8+effectiveBits-8*effectiveBytes)))
	expanded[128-effectiveBytes] = rc2PiTable[expanded[128-effectiveBytes]&mask]
	for i := 127 - effectiveBytes; i >= 0; i-- {
		expanded[i] = rc2PiTable[expanded[i+1]^expanded[i+effectiveBytes]]
	}
	c := &rc2{}
	for i := range c.k {
		c.k[i] = uint16(expanded[2*i]) | uint16(expanded[2*i+1])<<8
	}
	return c
}

func (c *rc2) BlockSize() int { return 8 }

func rc2Words(src []byte) [4]uint16 {
	return [4]uint16{uint16(src[0]) | uint16(src[1])<<8, uint16(src[2]) | uint16(src[3])<<8, uint16(src[4]) | uint16(src[5])<<8, uint16(src[6]) | uint16(src[7])<<8}
}

var rc2Shifts = [4]uint{1, 2, 3, 5}

func (c *rc2) Encrypt(dst, src []byte) {
	r := rc2Words(src)
	j := 0
	for _, rounds := range []int{5, 6, 5} {
		for n := 0; n < rounds; n++ {
			for i := 0; i < 4; i++ {
				r[i] += c.k[j] + (r[(i+3)%4] & r[(i+2)%4]) + (^r[(i+3)%4] & r[(i+1)%4])
				j++
				r[i] = r[i]<<rc2Shifts[i] | r[i]>>(16-rc2Shifts[i])
			}
		}
		if j < 64 {
			for i := 0; i < 4; i++ {
				r[i] += c.k[r[(i+3)%4]&63]
			}
		}
	}
	for i, w := range r {
		dst[2*i], dst[2*i+1] = byte(w), byte(w>>8)
	}
}

func (c *rc2) Decrypt(dst, src []byte) {
	r := rc2Words(src)
	j := 63
	for _, rounds := range []int{5, 6, 5} {
		for n := 0; n < rounds; n++ {
			for i := 3; i >= 0; i-- {
				r[i] = r[i]>>rc2Shifts[i] | r[i]<<(16-rc2Shifts[i])
				r[i] -= c.k[j] + (r[(i+3)%4] & r[(i+2)%4]) + (^r[(i+3)%4] & r[(i+1)%4])
				j--
			}
		}
		if j >= 0 {
			for i := 3; i >= 0; i-- {
				r[i] -= c.k[r[(i+3)%4]&63]
			}
		}
	}
	for i, w := range r {
		dst[2*i], dst[2*i+1] = byte(w), byte(w>>8)
	}
}
