package vault

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"hash"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var otpHashes = map[string]func() hash.Hash{"SHA1": sha1.New, "SHA256": sha256.New, "SHA512": sha512.New}

var (
	otpStrip   = regexp.MustCompile(`[\s-]`)
	otpBadSeed = regexp.MustCompile(`[^A-Z2-7]`)
)

// Seed kept of an authenticator key, a base32 secret or an otpauth://totp link, its digits, period and hash appended when they are not
// 6, 30 and SHA1; empty for none
func NormalizeOTP(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	digits, period, algo := 6, 30, "SHA1"
	if strings.HasPrefix(strings.ToLower(value), "otpauth://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", &Error{"authenticator key must be a base32 secret or an otpauth:// URI"}
		}
		if strings.ToLower(parsed.Host) != "totp" {
			return "", &Error{"only otpauth://totp links are supported (counter-based HOTP is not)"}
		}
		query := map[string]string{}
		for _, pair := range strings.Split(parsed.RawQuery, "&") {
			name, raw, _ := strings.Cut(pair, "=")
			name, _ = url.QueryUnescape(name)
			raw, _ = url.QueryUnescape(raw)
			if raw == "" {
				continue
			}
			if _, seen := query[strings.ToLower(name)]; !seen {
				query[strings.ToLower(name)] = raw
			}
		}
		value = query["secret"]
		whole := func(key string, def int) (int, bool) {
			raw, ok := query[key]
			if !ok {
				return def, true
			}
			n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
			return int(n), err == nil
		}
		var okDigits, okPeriod bool
		if digits, okDigits = whole("digits", digits); okDigits {
			period, okPeriod = whole("period", period)
		}
		if !okDigits || !okPeriod {
			return "", &Error{"otpauth:// digits/period must be integers"}
		}
		if a, ok := query["algorithm"]; ok {
			algo = a
		}
		algo = strings.ReplaceAll(strings.ToUpper(algo), "-", "")
		if _, known := otpHashes[algo]; digits < 6 || digits > 8 || period <= 0 || !known {
			return "", &Error{"unsupported otpauth:// parameters (digits 6-8, period > 0, SHA1/SHA256/SHA512)"}
		}
	}
	seed := strings.TrimRight(strings.ToUpper(otpStrip.ReplaceAllString(value, "")), "=")
	if seed == "" || otpBadSeed.MatchString(seed) {
		return "", &Error{"authenticator key must be a base32 secret or an otpauth:// URI"}
	}
	if digits == 6 && period == 30 && algo == "SHA1" {
		return seed, nil
	}
	return fmt.Sprintf("%s|%d|%d|%s", seed, digits, period, algo), nil
}

// RFC 6238 code of a kept seed at a time
func totp(seed string, at time.Time) (string, error) {
	digits, period, newHash := 6, 30, sha1.New
	if strings.Contains(seed, "|") {
		parts := strings.SplitN(seed, "|", 4)
		if len(parts) != 4 {
			return "", fmt.Errorf("the stored OTP seed has %d of its 4 parts", len(parts))
		}
		d, errD := strconv.Atoi(strings.TrimSpace(parts[1]))
		p, errP := strconv.Atoi(strings.TrimSpace(parts[2]))
		if errD != nil || errP != nil || p <= 0 {
			return "", fmt.Errorf("the stored OTP seed digits or period is not a whole number")
		}
		seed, digits, period = parts[0], d, p
		if h, ok := otpHashes[strings.ToUpper(parts[3])]; ok {
			newHash = h
		}
	}
	key, err := base32.StdEncoding.DecodeString(strings.ToUpper(seed) + strings.Repeat("=", (8-len(seed)%8)%8))
	if err != nil {
		return "", fmt.Errorf("the stored OTP seed is not base32: %v", err)
	}
	counter := uint64(int64(math.Floor(float64(at.UnixNano()) / 1e9 / float64(period))))
	mac := hmac.New(newHash, key)
	mac.Write(binary.BigEndian.AppendUint64(nil, counter))
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 0x0f
	code := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", digits, code%uint32(math.Pow10(digits))), nil
}

// Scheme, host and port of a URL or origin, the port left out where it is the scheme's own
func NormalizeOrigin(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", &Error{"origin is required"}
	}
	if !strings.Contains(value, "://") {
		return "", &Error{fmt.Sprintf("origin must include a scheme (got %q)", value)}
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", &Error{fmt.Sprintf("could not parse origin from %q", value)}
	}
	scheme, host := strings.ToLower(parsed.Scheme), strings.ToLower(parsed.Hostname())
	if scheme == "" || host == "" {
		return "", &Error{fmt.Sprintf("could not parse origin from %q", value)}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := parsed.Port()
	if port == "" {
		return scheme + "://" + host, nil
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return "", &Error{fmt.Sprintf("invalid port in origin %q", value)}
	}
	if (scheme == "http" && n == 80) || (scheme == "https" && n == 443) {
		return scheme + "://" + host, nil
	}
	return fmt.Sprintf("%s://%s:%d", scheme, host, n), nil
}

var passwordEcho = regexp.MustCompile(`(password['"]?\s*[:=]\s*)\S+`)

// Text with every secret value of three characters or more, and every echo of a password, masked: what a page or an error may show of a
// fill
func Scrub(text string, secret map[string]string) string {
	for _, value := range secret {
		if len([]rune(value)) >= 3 {
			text = strings.ReplaceAll(text, value, "[REDACTED]")
		}
	}
	return passwordEcho.ReplaceAllString(text, "${1}[REDACTED]")
}
