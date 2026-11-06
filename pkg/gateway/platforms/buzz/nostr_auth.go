package buzz

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	fieldOrder = new(big.Int).Sub(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), new(big.Int).Lsh(big.NewInt(1), 32)), big.NewInt(977))
	curveOrder = mustHexInt("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141")
	generator  = &point{mustHexInt("79BE667EF9DCBBAC55A06295CE870B07029BFCDB2DCE28D959F2815B16F81798"), mustHexInt("483ADA7726A3C4655DA4FBFC0E1108A8FD17B448A68554199C47D08FFB10D4B8")}
)

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var bech32Generators = [5]uint32{0x3B6A57B2, 0x26508E6D, 0x1EA119FA, 0x3D4233DD, 0x2A1462B3}

type point struct{ x, y *big.Int }

// Big integer of a constant hex literal
func mustHexInt(s string) *big.Int {
	n, _ := new(big.Int).SetString(s, 16)
	return n
}

// Checksum state of the bech32 polymod over five bit values
func bech32Polymod(values []int) uint32 {
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
func bech32HRPExpand(hrp string) []int {
	out := make([]int, 0, len(hrp)*2+1)
	for _, c := range hrp {
		out = append(out, int(c)>>5)
	}
	out = append(out, 0)
	for _, c := range hrp {
		out = append(out, int(c)&31)
	}
	return out
}

// Thirty two key bytes of a checksummed lowercase or uppercase nsec string
func decodeNsec(value string) ([]byte, error) {
	if pystr.Lower(value) != value && pystr.Upper(value) != value {
		return nil, errors.New("nsec cannot mix upper- and lowercase")
	}
	normalized := []rune(pystr.Lower(value))
	separator := strings.LastIndex(string(normalized), "1")
	if separator >= 0 {
		separator = len([]rune(string(normalized)[:separator]))
	}
	if separator < 1 || separator+7 > len(normalized) {
		return nil, errors.New("invalid nsec encoding")
	}
	hrp := string(normalized[:separator])
	if hrp != "nsec" {
		return nil, errors.New("private key must use the nsec prefix")
	}
	data := make([]int, 0, len(normalized)-separator-1)
	for _, c := range normalized[separator+1:] {
		idx := strings.IndexRune(bech32Charset, c)
		if idx < 0 {
			return nil, errors.New("invalid character in nsec")
		}
		data = append(data, idx)
	}
	if bech32Polymod(append(bech32HRPExpand(hrp), data...)) != 1 {
		return nil, errors.New("invalid nsec checksum")
	}
	accumulator := new(big.Int)
	bits := 0
	var decoded []byte
	for _, v := range data[:len(data)-6] {
		accumulator.Lsh(accumulator, 5).Or(accumulator, big.NewInt(int64(v)))
		bits += 5
		for bits >= 8 {
			bits -= 8
			decoded = append(decoded, byte(new(big.Int).Rsh(accumulator, uint(bits)).Uint64()&0xFF))
		}
	}
	if bits > 0 {
		mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bits)), big.NewInt(1))
		if new(big.Int).And(accumulator, mask).Sign() != 0 {
			return nil, errors.New("non-zero nsec padding")
		}
	}
	if len(decoded) != 32 {
		return nil, errors.New("nsec must encode exactly 32 bytes")
	}
	return decoded, nil
}

// Secret scalar of a 64 hex character or nsec key inside the secp256k1 range
func decodePrivateKey(value string) (*big.Int, error) {
	raw := pystr.Strip(value)
	var keyBytes []byte
	if strings.HasPrefix(pystr.Lower(raw), "nsec1") {
		b, err := decodeNsec(raw)
		if err != nil {
			return nil, err
		}
		keyBytes = b
	} else {
		b, err := pystr.BytesFromHex(raw)
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

// Affine sum of two curve points, nil standing for the point at infinity
func pointAdd(left, right *point) *point {
	if left == nil || right == nil {
		if left == nil {
			return right
		}
		return left
	}
	var slope *big.Int
	exp := new(big.Int).Sub(fieldOrder, big.NewInt(2))
	if left.x.Cmp(right.x) == 0 {
		if new(big.Int).Mod(new(big.Int).Add(left.y, right.y), fieldOrder).Sign() == 0 {
			return nil
		}
		num := new(big.Int).Mul(big.NewInt(3), new(big.Int).Mul(left.x, left.x))
		den := new(big.Int).Exp(new(big.Int).Mod(new(big.Int).Mul(big.NewInt(2), left.y), fieldOrder), exp, fieldOrder)
		slope = num.Mul(num, den)
	} else {
		num := new(big.Int).Sub(right.y, left.y)
		den := new(big.Int).Exp(new(big.Int).Mod(new(big.Int).Sub(right.x, left.x), fieldOrder), exp, fieldOrder)
		slope = num.Mul(num, den)
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
	s := new(big.Int).Set(scalar)
	for s.Sign() > 0 {
		if s.Bit(0) == 1 {
			result = pointAdd(result, addend)
		}
		addend = pointAdd(addend, addend)
		s.Rsh(s, 1)
	}
	return result
}

// BIP340 tagged SHA256 of the payload
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
func PublicKeyHex(privateKey string) (string, error) {
	secret, err := decodePrivateKey(privateKey)
	if err != nil {
		return "", err
	}
	p := pointMultiply(secret, generator)
	if p == nil {
		return "", errors.New("invalid private key")
	}
	return hex.EncodeToString(be32(p.x)), nil
}

// BIP340 signature of a 32 byte message, fresh auxiliary randomness when aux is nil
func SchnorrSign(message []byte, privateKey string, aux []byte) ([]byte, error) {
	if len(message) != 32 {
		return nil, errors.New("BIP-340 signs a 32-byte message")
	}
	secret, err := decodePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	public := pointMultiply(secret, generator)
	if public == nil {
		return nil, errors.New("invalid private key")
	}
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
	if noncePoint == nil {
		return nil, errors.New("BIP-340 produced an invalid nonce point")
	}
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

// Four string NIP-OA owner attestation tag from JSON text or a decoded list
func ParseAuthTag(raw any, label string) ([]string, error) {
	if text, ok := raw.(string); ok {
		decoded, err := pyjson.Loads(text)
		if err != nil {
			return nil, fmt.Errorf("%s is not valid JSON", label)
		}
		raw = decoded
	}
	var items []any
	switch v := raw.(type) {
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	}
	bad := fmt.Errorf("%s must be a four-string auth tag", label)
	if len(items) != 4 || items[0] != "auth" {
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
	return out, nil
}

// Signed kind 22242 relay authentication event
type AuthEvent struct {
	ID        string     `json:"id"`
	Pubkey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

// Inputs of a relay authentication event, CreatedAt nil meaning now and AuxiliaryRandomness nil meaning fresh
type AuthEventParams struct {
	PrivateKey          string
	Challenge           string
	RelayURL            string
	AuthTagJSON         string
	CreatedAt           *int64
	AuxiliaryRandomness []byte
}

// NIP-42 auth event for the relay challenge, with the owner attestation tag when configured
func BuildAuthEvent(p AuthEventParams) (AuthEvent, error) {
	tags := [][]string{{"relay", p.RelayURL}, {"challenge", p.Challenge}}
	if pystr.Strip(p.AuthTagJSON) != "" {
		tag, err := ParseAuthTag(p.AuthTagJSON, "BUZZ_AUTH_TAG")
		if err != nil {
			return AuthEvent{}, err
		}
		tags = append(tags, tag)
	}
	pubkey, err := PublicKeyHex(p.PrivateKey)
	if err != nil {
		return AuthEvent{}, err
	}
	timestamp := time.Now().Unix()
	if p.CreatedAt != nil {
		timestamp = *p.CreatedAt
	}
	tagList := make([]any, len(tags))
	for i, tag := range tags {
		row := make([]any, len(tag))
		for j, s := range tag {
			row[j] = s
		}
		tagList[i] = row
	}
	serialized, err := pyjson.Dumps([]any{0, pubkey, timestamp, 22242, tagList, ""}, pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
	if err != nil {
		return AuthEvent{}, err
	}
	eventID := sha256.Sum256([]byte(serialized))
	sig, err := SchnorrSign(eventID[:], p.PrivateKey, p.AuxiliaryRandomness)
	if err != nil {
		return AuthEvent{}, err
	}
	return AuthEvent{ID: hex.EncodeToString(eventID[:]), Pubkey: pubkey, CreatedAt: timestamp, Kind: 22242, Tags: tags, Content: "", Sig: hex.EncodeToString(sig)}, nil
}
