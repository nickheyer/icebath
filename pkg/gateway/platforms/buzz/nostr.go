package buzz

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	authEventKind = 22242
)

var (
	fieldOrder = mustHexInt("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEFFFFFC2F")
	curveOrder = mustHexInt("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141")
	generator  = &point{
		mustHexInt("79BE667EF9DCBBAC55A06295CE870B07029BFCDB2DCE28D959F2815B16F81798"),
		mustHexInt("483ADA7726A3C4655DA4FBFC0E1108A8FD17B448A68554199C47D08FFB10D4B8"),
	}
	bech32Generators = [5]uint32{0x3B6A57B2, 0x26508E6D, 0x1EA119FA, 0x3D4233DD, 0x2A1462B3}
)

// Affine secp256k1 point, nil standing for the point at infinity
type point struct{ x, y *big.Int }

// Big integer of a constant hex literal
func mustHexInt(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		panic("buzz: bad curve constant " + s)
	}
	return n
}

// Checksum state of the bech32 polymod over five bit values
func bech32Polymod(values []byte) uint32 {
	checksum := uint32(1)
	for _, value := range values {
		top := checksum >> 25
		checksum = (checksum&0x1FFFFFF)<<5 ^ uint32(value)
		for i, g := range bech32Generators {
			if (top>>i)&1 == 1 {
				checksum ^= g
			}
		}
	}
	return checksum
}

// High bits, a zero separator, then low bits of each human readable part character
func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

// Regroups bit groups, false when unpadded input leaves nonzero or overlong padding
func convertBits(data []byte, from, to uint, pad bool) ([]byte, bool) {
	acc, bits := uint32(0), uint(0)
	maxv := uint32(1)<<to - 1
	out := []byte{}
	for _, value := range data {
		if uint32(value)>>from != 0 {
			return nil, false
		}
		acc = acc<<from | uint32(value)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxv))
		}
	}
	if pad && bits > 0 {
		out = append(out, byte(acc<<(to-bits)&maxv))
	} else if !pad && (bits >= from || acc<<(to-bits)&maxv != 0) {
		return nil, false
	}
	return out, true
}

// Five bit values of a bech32 data part, false on a character outside the charset
func bech32Values(data string) ([]byte, bool) {
	out := make([]byte, 0, len(data))
	for _, c := range data {
		idx := strings.IndexRune(bech32Charset, c)
		if idx < 0 {
			return nil, false
		}
		out = append(out, byte(idx))
	}
	return out, true
}

// Bech32 string of the payload bytes under the human readable part
func bech32Encode(hrp string, payload []byte) string {
	data, _ := convertBits(payload, 8, 5, true)
	values := append(bech32HRPExpand(hrp), data...)
	polymod := bech32Polymod(append(values, 0, 0, 0, 0, 0, 0)) ^ 1
	var b strings.Builder
	b.WriteString(hrp)
	b.WriteByte('1')
	for _, d := range data {
		b.WriteByte(bech32Charset[d])
	}
	for i := 0; i < 6; i++ {
		b.WriteByte(bech32Charset[(polymod>>(5*(5-i)))&31])
	}
	return b.String()
}

// Npub of a 64 character hex public key, empty when the hex is not 32 bytes
func hexToNpub(pubkeyHex string) string {
	raw, err := hex.DecodeString(pubkeyHex)
	if err != nil || len(raw) != 32 {
		return ""
	}
	return bech32Encode("npub", raw)
}

// Lowercase hex public key of an npub, empty when it is not a valid npub
func npubToHex(npub string) string {
	npub = strings.ToLower(strings.TrimSpace(npub))
	if !strings.HasPrefix(npub, "npub1") {
		return ""
	}
	data, ok := bech32Values(npub[len("npub1"):])
	if !ok || bech32Polymod(append(bech32HRPExpand("npub"), data...)) != 1 || len(data) < 6 {
		return ""
	}
	decoded, ok := convertBits(data[:len(data)-6], 5, 8, false)
	if !ok || len(decoded) != 32 {
		return ""
	}
	return hex.EncodeToString(decoded)
}

// Thirty two key bytes of a checksummed nsec written in one case
func decodeNsec(value string) ([]byte, error) {
	if strings.ToLower(value) != value && strings.ToUpper(value) != value {
		return nil, errors.New("nsec cannot mix upper- and lowercase")
	}
	normalized := strings.ToLower(value)
	separator := strings.LastIndex(normalized, "1")
	if separator < 1 || separator+7 > len(normalized) {
		return nil, errors.New("invalid nsec encoding")
	}
	hrp := normalized[:separator]
	if hrp != "nsec" {
		return nil, errors.New("private key must use the nsec prefix")
	}
	data, ok := bech32Values(normalized[separator+1:])
	if !ok {
		return nil, errors.New("invalid character in nsec")
	}
	if bech32Polymod(append(bech32HRPExpand(hrp), data...)) != 1 {
		return nil, errors.New("invalid nsec checksum")
	}
	acc, bits := uint32(0), uint(0)
	decoded := []byte{}
	for _, v := range data[:len(data)-6] {
		acc = (acc<<5 | uint32(v)) & 0xFFFF
		bits += 5
		for bits >= 8 {
			bits -= 8
			decoded = append(decoded, byte(acc>>bits))
		}
	}
	if bits > 0 && acc&(1<<bits-1) != 0 {
		return nil, errors.New("non-zero nsec padding")
	}
	if len(decoded) != 32 {
		return nil, errors.New("nsec must encode exactly 32 bytes")
	}
	return decoded, nil
}

// Secret scalar of a hex or nsec private key inside the secp256k1 range
func decodePrivateKey(value string) (*big.Int, error) {
	raw := strings.TrimSpace(value)
	var keyBytes []byte
	if strings.HasPrefix(strings.ToLower(raw), "nsec1") {
		b, err := decodeNsec(raw)
		if err != nil {
			return nil, err
		}
		keyBytes = b
	} else {
		b, err := hex.DecodeString(raw)
		if err != nil {
			return nil, errors.New("private key must be 64 hex characters or nsec")
		}
		if len(b) != 32 {
			return nil, errors.New("private key must be 32 bytes")
		}
		keyBytes = b
	}
	key := new(big.Int).SetBytes(keyBytes)
	if key.Sign() < 1 || key.Cmp(curveOrder) >= 0 {
		return nil, errors.New("private key is outside the secp256k1 range")
	}
	return key, nil
}

// Affine sum of two curve points
func pointAdd(left, right *point) *point {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	slope := new(big.Int)
	if left.x.Cmp(right.x) == 0 {
		sum := new(big.Int).Add(left.y, right.y)
		if sum.Mod(sum, fieldOrder).Sign() == 0 {
			return nil
		}
		num := new(big.Int).Mul(left.x, left.x)
		num.Mul(num, big.NewInt(3))
		den := new(big.Int).Lsh(left.y, 1)
		den.ModInverse(den.Mod(den, fieldOrder), fieldOrder)
		slope.Mul(num, den)
	} else {
		num := new(big.Int).Sub(right.y, left.y)
		den := new(big.Int).Sub(right.x, left.x)
		den.ModInverse(den.Mod(den, fieldOrder), fieldOrder)
		slope.Mul(num, den)
	}
	slope.Mod(slope, fieldOrder)
	x3 := new(big.Int).Mul(slope, slope)
	x3.Sub(x3, left.x).Sub(x3, right.x).Mod(x3, fieldOrder)
	y3 := new(big.Int).Sub(left.x, x3)
	y3.Mul(y3, slope).Sub(y3, left.y).Mod(y3, fieldOrder)
	return &point{x3, y3}
}

// Double and add scalar multiple of a curve point
func pointMultiply(scalar *big.Int, p *point) *point {
	var result *point
	addend := p
	for i := 0; i < scalar.BitLen(); i++ {
		if scalar.Bit(i) == 1 {
			result = pointAdd(result, addend)
		}
		addend = pointAdd(addend, addend)
	}
	return result
}

// BIP-340 tagged SHA-256 of the concatenated payloads
func taggedHash(tag string, payload ...[]byte) []byte {
	tagHash := sha256.Sum256([]byte(tag))
	h := sha256.New()
	h.Write(tagHash[:])
	h.Write(tagHash[:])
	for _, p := range payload {
		h.Write(p)
	}
	return h.Sum(nil)
}

// Thirty two byte big endian encoding of a field element or scalar
func be32(n *big.Int) []byte { return n.FillBytes(make([]byte, 32)) }

// X only public key hex of a private key
func publicKeyHex(privateKey string) (string, error) {
	secret, err := decodePrivateKey(privateKey)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(be32(pointMultiply(secret, generator).x)), nil
}

// BIP-340 signature of a 32 byte message, fresh auxiliary randomness when aux is nil
func schnorrSign(message []byte, privateKey string, aux []byte) ([]byte, error) {
	if len(message) != 32 {
		return nil, errors.New("BIP-340 signs a 32-byte message")
	}
	secret, err := decodePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	public := pointMultiply(secret, generator)
	publicX := be32(public.x)
	adjusted := new(big.Int).Set(secret)
	if public.y.Bit(0) == 1 {
		adjusted.Sub(curveOrder, secret)
	}
	if aux == nil {
		aux = make([]byte, 32)
		if _, err := rand.Read(aux); err != nil {
			return nil, err
		}
	}
	if len(aux) != 32 {
		return nil, errors.New("auxiliary randomness must be 32 bytes")
	}
	masked := be32(new(big.Int).Xor(adjusted, new(big.Int).SetBytes(taggedHash("BIP0340/aux", aux))))
	nonce := new(big.Int).SetBytes(taggedHash("BIP0340/nonce", masked, publicX, message))
	nonce.Mod(nonce, curveOrder)
	if nonce.Sign() == 0 {
		return nil, errors.New("BIP-340 produced a zero nonce")
	}
	noncePoint := pointMultiply(nonce, generator)
	nonceX := be32(noncePoint.x)
	if noncePoint.y.Bit(0) == 1 {
		nonce.Sub(curveOrder, nonce)
	}
	challenge := new(big.Int).SetBytes(taggedHash("BIP0340/challenge", nonceX, publicX, message))
	challenge.Mod(challenge, curveOrder)
	s := new(big.Int).Mul(challenge, adjusted)
	s.Add(s, nonce).Mod(s, curveOrder)
	return append(nonceX, be32(s)...), nil
}

// Invalid owner attestation configuration, surfaced to the user as a config error
type authTagError struct{ msg string }

func (e *authTagError) Error() string { return e.msg }

// Four string NIP-OA owner attestation tag from JSON text or a decoded list
func parseAuthTag(raw any, label string) ([]string, error) {
	if text, ok := raw.(string); ok {
		var decoded any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			return nil, &authTagError{label + " is not valid JSON"}
		}
		raw = decoded
	}
	bad := &authTagError{label + " must be a four-string auth tag"}
	items, ok := raw.([]any)
	if !ok || len(items) != 4 {
		return nil, bad
	}
	out := make([]string, 4)
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, bad
		}
		out[i] = s
	}
	if out[0] != "auth" {
		return nil, bad
	}
	return out, nil
}

// Signed Nostr event as it travels on the relay wire
type nostrEvent struct {
	ID        string     `json:"id"`
	Pubkey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

// JSON string literal escaped the way NIP-01 event serialization requires
func nostrJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
}

// Event id of the NIP-01 serialization of pubkey, timestamp, kind, tags and content
func nostrEventID(pubkey string, createdAt int64, kind int, tags [][]string, content string) [32]byte {
	var b strings.Builder
	b.WriteString("[0,")
	nostrJSONString(&b, pubkey)
	fmt.Fprintf(&b, ",%d,%d,[", createdAt, kind)
	for i, tag := range tags {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('[')
		for j, s := range tag {
			if j > 0 {
				b.WriteByte(',')
			}
			nostrJSONString(&b, s)
		}
		b.WriteByte(']')
	}
	b.WriteString("],")
	nostrJSONString(&b, content)
	b.WriteByte(']')
	return sha256.Sum256([]byte(b.String()))
}

// Signed NIP-42 kind 22242 answer to a relay challenge, carrying the owner attestation tag when configured
func buildAuthEvent(privateKey, challenge, relayURL, authTagJSON string) (nostrEvent, error) {
	tags := [][]string{{"relay", relayURL}, {"challenge", challenge}}
	if strings.TrimSpace(authTagJSON) != "" {
		tag, err := parseAuthTag(authTagJSON, "BUZZ_AUTH_TAG")
		if err != nil {
			return nostrEvent{}, err
		}
		tags = append(tags, tag)
	}
	pubkey, err := publicKeyHex(privateKey)
	if err != nil {
		return nostrEvent{}, err
	}
	createdAt := time.Now().Unix()
	id := nostrEventID(pubkey, createdAt, authEventKind, tags, "")
	sig, err := schnorrSign(id[:], privateKey, nil)
	if err != nil {
		return nostrEvent{}, err
	}
	return nostrEvent{ID: hex.EncodeToString(id[:]), Pubkey: pubkey, CreatedAt: createdAt, Kind: authEventKind,
		Tags: tags, Content: "", Sig: hex.EncodeToString(sig)}, nil
}
