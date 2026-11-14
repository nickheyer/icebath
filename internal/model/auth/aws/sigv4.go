package aws

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	BedrockService  = "bedrock"
	sigAlgorithm    = "AWS4-HMAC-SHA256"
	sigTimeLayout   = "20060102T150405Z"
	sigDateLayout   = "20060102"
	sigTerminator   = "aws4_request"
	emptyPayloadSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

var ErrUnsignable = errors.New("aws: request cannot be signed without a URL and a complete key pair")

var unsignedHeaders = map[string]bool{
	"expect": true, "transfer-encoding": true, "user-agent": true, "x-amzn-trace-id": true,
	"connection": true, "accept-encoding": true, "content-length": true,
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func signingKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, sigTerminator)
}

func uriEscape(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&15])
		}
	}
	return b.String()
}

func removeDotSegments(path string) string {
	if path == "" {
		return "/"
	}
	var out []string
	for _, seg := range strings.Split(path, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	first, last := "", ""
	if strings.HasPrefix(path, "/") {
		first = "/"
	}
	if strings.HasSuffix(path, "/") && len(out) > 0 {
		last = "/"
	}
	return first + strings.Join(out, "/") + last
}

func canonicalQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	type pair struct{ k, v string }
	var pairs []pair
	for _, part := range strings.Split(rawQuery, "&") {
		k, v, _ := strings.Cut(part, "=")
		pairs = append(pairs, pair{k, v})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

func hostOf(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	scheme := strings.ToLower(u.Scheme)
	if port != "" && !(scheme == "https" && port == "443") && !(scheme == "http" && port == "80") {
		if strings.HasPrefix(host, "[") {
			return host + ":" + port
		}
		return net.JoinHostPort(host, port)
	}
	return host
}

func signRequest(r *http.Request, body []byte, creds Credentials, service, region string, now time.Time) error {
	if r.URL == nil || !creds.complete() {
		return ErrUnsignable
	}
	ts := now.UTC()
	r.Header.Del("Authorization")
	r.Header.Del("X-Amz-Security-Token")
	if r.Header.Get("Date") != "" {
		r.Header.Set("Date", ts.Format("Mon, 02 Jan 2006 15:04:05 -0000"))
		r.Header.Del("X-Amz-Date")
	} else {
		r.Header.Set("X-Amz-Date", ts.Format(sigTimeLayout))
	}
	if creds.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	if r.Host == "" {
		r.Host = hostOf(r.URL)
	}
	values := map[string]string{"host": r.Host}
	for name, vals := range r.Header {
		lname := strings.ToLower(name)
		if unsignedHeaders[lname] || lname == "host" {
			continue
		}
		folded := make([]string, len(vals))
		for i, v := range vals {
			folded[i] = strings.Join(strings.Fields(v), " ")
		}
		values[lname] = strings.Join(folded, ",")
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var headers strings.Builder
	for _, name := range names {
		headers.WriteString(name + ":" + values[name] + "\n")
	}
	signed := strings.Join(names, ";")
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if payload == "" {
		payload = emptyPayloadSHA
		if len(body) > 0 {
			payload = sha256Hex(body)
		}
	}
	canonical := strings.Join([]string{
		strings.ToUpper(r.Method), uriEscape(removeDotSegments(r.URL.EscapedPath()), true), canonicalQuery(r.URL.RawQuery),
		headers.String(), signed, payload,
	}, "\n")
	date := ts.Format(sigDateLayout)
	scope := date + "/" + region + "/" + service + "/" + sigTerminator
	toSign := strings.Join([]string{sigAlgorithm, ts.Format(sigTimeLayout), scope, sha256Hex([]byte(canonical))}, "\n")
	signature := hex.EncodeToString(hmacSHA256(signingKey(creds.SecretAccessKey, date, region, service), toSign))
	r.Header.Set("Authorization", sigAlgorithm+" Credential="+creds.AccessKeyID+"/"+scope+", SignedHeaders="+signed+", Signature="+signature)
	return nil
}

type Signer struct {
	chain  *Chain
	region string
	bearer Env
}

func (c *Chain) Signer(region string, bearer Env) *Signer {
	region = strings.TrimSpace(region)
	if region == "" {
		region = Region(ProcessEnv)
	}
	return &Signer{chain: c, region: region, bearer: bearer}
}

func (s *Signer) Region() string { return s.region }

func (s *Signer) Authorize(ctx context.Context, r *http.Request, body []byte) error {
	if s.bearer != nil {
		if token := BearerToken(s.bearer); token != "" {
			r.Header.Del("X-Amz-Date")
			r.Header.Del("X-Amz-Security-Token")
			r.Header.Set("Authorization", "Bearer "+token)
			return nil
		}
	}
	creds, err := s.chain.Retrieve(ctx)
	if err != nil {
		return err
	}
	return signRequest(r, body, creds, BedrockService, s.region, time.Now())
}
