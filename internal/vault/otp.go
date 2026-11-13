package vault

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Error for a stored authenticator key no code can be generated from
var errBadSeed = errors.New("stored authenticator key cannot generate a code")

// Most digits a code can carry, a 31 bit truncated HMAC having at most ten
const maxOTPDigits = 10

// Hash an authenticator algorithm name selects
func otpHash(name string) (func() hash.Hash, bool) {
	switch name {
	case "SHA1":
		return sha1.New, true
	case "SHA256":
		return sha256.New, true
	case "SHA512":
		return sha512.New, true
	}
	return nil, false
}

// Stored form of a base32 seed or otpauth totp URI, parameters appended only when not 6 digits, 30 seconds, SHA1
func normalizeOTPSecret(value string) (string, error) {
	value = trimSpace(value)
	if value == "" {
		return "", nil
	}
	digits, period, algo := big.NewInt(6), big.NewInt(30), "SHA1"
	if strings.HasPrefix(lowerFull(value), "otpauth://") {
		parts, err := splitURL(value)
		if err != nil {
			return "", err
		}
		if lowerFull(parts.netloc) != "totp" {
			return "", failure(ErrInvalid, "only otpauth://totp links are supported (counter-based HOTP is not)")
		}
		qs := queryFirsts(parts.query)
		value = qs["secret"]
		for _, field := range []struct {
			name   string
			target **big.Int
		}{{"digits", &digits}, {"period", &period}} {
			if text, ok := qs[field.name]; ok {
				n, ok := parseInteger(text)
				if !ok {
					return "", failure(ErrInvalid, "otpauth:// digits/period must be integers")
				}
				*field.target = n
			}
		}
		if a, ok := qs["algorithm"]; ok {
			algo = a
		}
		algo = strings.ReplaceAll(upperFull(algo), "-", "")
		_, known := otpHash(algo)
		if !digits.IsInt64() || digits.Int64() < 6 || digits.Int64() > 8 || period.Sign() <= 0 || !known {
			return "", failure(ErrInvalid, "unsupported otpauth:// parameters (digits 6-8, period > 0, SHA1/SHA256/SHA512)")
		}
	}
	seed := strings.TrimRight(upperFull(strings.Map(func(r rune) rune {
		if r == '-' || isSpace(r) {
			return -1
		}
		return r
	}, value)), "=")
	if seed == "" || strings.ContainsFunc(seed, func(r rune) bool { return !(r >= 'A' && r <= 'Z' || r >= '2' && r <= '7') }) {
		return "", failure(ErrInvalid, "authenticator key must be a base32 secret or an otpauth:// URI")
	}
	if digits.Int64() == 6 && period.Int64() == 30 && algo == "SHA1" {
		return seed, nil
	}
	return fmt.Sprintf("%s|%s|%s|%s", seed, digits, period, algo), nil
}

// Integer text with optional surrounding whitespace, a sign and single underscores between digits
func parseInteger(text string) (*big.Int, bool) {
	t := trimSpace(text)
	sign := ""
	if t != "" && (t[0] == '+' || t[0] == '-') {
		sign, t = t[:1], t[1:]
	}
	if t == "" || t[0] == '_' || t[len(t)-1] == '_' || strings.Contains(t, "__") {
		return nil, false
	}
	n, ok := new(big.Int).SetString(sign+strings.ReplaceAll(t, "_", ""), 10)
	return n, ok
}

// RFC 6238 code of a stored seed at time at, honouring any digits, period and algorithm it carries
func totp(seed string, at time.Time) (string, error) {
	digits, period, newHash := int64(6), big.NewInt(30), sha1.New
	if strings.Contains(seed, "|") {
		parts := strings.SplitN(seed, "|", 4)
		if len(parts) != 4 {
			return "", errBadSeed
		}
		d, okD := parseInteger(parts[1])
		p, okP := parseInteger(parts[2])
		if !okD || !okP || !d.IsInt64() || d.Sign() < 0 || d.Int64() > maxOTPDigits || p.Sign() <= 0 {
			return "", errBadSeed
		}
		seed, digits, period = parts[0], d.Int64(), p
		if h, ok := otpHash(upperFull(parts[3])); ok {
			newHash = h
		}
	}
	padded := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, seed) + strings.Repeat("=", (8-len(seed)%8)%8)
	key, err := base32.StdEncoding.DecodeString(padded)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errBadSeed, err)
	}
	step := new(big.Int).Mul(period, big.NewInt(int64(time.Second)))
	counter := new(big.Int).Div(big.NewInt(at.UnixNano()), step)
	if counter.Sign() < 0 {
		return "", fmt.Errorf("%w: the clock reads a time before 1970", errBadSeed)
	}
	mac := hmac.New(newHash, key)
	mac.Write(binary.BigEndian.AppendUint64(nil, counter.Uint64()))
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 0x0f
	code := uint64(binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff)
	if digits < 10 {
		modulus := uint64(1)
		for range digits {
			modulus *= 10
		}
		code %= modulus
	}
	text := strconv.FormatUint(code, 10)
	if pad := int(digits) - len(text); pad > 0 {
		text = strings.Repeat("0", pad) + text
	}
	return text, nil
}
