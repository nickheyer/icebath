package provider

import (
	"bytes"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Reports whether a base URL is OpenAI's own Codex backend rather than a proxy
func isOfficialCodexBaseURL(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "chatgpt.com") {
		return false
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return false
	}
	path := strings.TrimRight(parsed.Path, "/")
	return path == "/backend-api/codex" || strings.HasPrefix(path, "/backend-api/codex/")
}

// Identity and account headers for the Codex backend, icebath identity on the official host
func codexCloudflareHeaders(accessToken, baseURL string) map[string]string {
	headers := map[string]string{"User-Agent": "codex_cli_rs/0.0.0 (Icebath Agent)", "originator": "codex_cli_rs"}
	if isOfficialCodexBaseURL(baseURL) {
		headers = map[string]string{"User-Agent": userAgent("IcebathAgent"), "originator": "icebath-agent"}
	}
	maps.Copy(headers, codexAccountHeaders(accessToken))
	return headers
}

// Codex backend identity and account headers for an access token and base url
func CodexCloudflareHeaders(accessToken, baseURL string) map[string]string {
	return codexCloudflareHeaders(accessToken, baseURL)
}

// Workspace account and residency headers decoded from the OAuth JWT, absent on a malformed token
func codexAccountHeaders(accessToken string) map[string]string {
	headers := map[string]string{}
	if strings.TrimSpace(accessToken) == "" {
		return headers
	}
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return headers
	}
	claims := DecodeJWTClaims(parts[0] + "." + parts[1] + ".x")
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	if auth == nil {
		return headers
	}
	if acct, ok := auth["chatgpt_account_id"].(string); ok && acct != "" {
		headers["ChatGPT-Account-ID"] = acct
	}
	residency, _ := auth["chatgpt_data_residency"].(string)
	if residency == "" {
		residency, _ = auth["chatgpt_compute_residency"].(string)
	}
	if residency = strings.TrimSpace(residency); residency != "" {
		headers["x-openai-internal-codex-residency"] = residency
	}
	return headers
}

// Zstd compresses a Responses POST body for the official Codex backend and stamps the encoding header
func codexCompressRequest(req *http.Request, baseURL string, body []byte) []byte {
	if !isOfficialCodexBaseURL(baseURL) || req.Method != http.MethodPost ||
		!strings.HasSuffix(req.URL.Path, "/responses") || req.Header.Get("Content-Encoding") != "" {
		return body
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return body
	}
	compressed := enc.EncodeAll(body, make([]byte, 0, len(body)))
	enc.Close()
	req.Body = io.NopCloser(bytes.NewReader(compressed))
	req.ContentLength = int64(len(compressed))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(compressed)), nil }
	req.Header.Set("Content-Encoding", "zstd")
	return compressed
}

// Keeps the required Codex identity headers after user or provider header overrides on the official host
func applyRequiredCodexHeaders(headers map[string]string, accessToken, baseURL string) map[string]string {
	if !isOfficialCodexBaseURL(baseURL) {
		return headers
	}
	required := codexCloudflareHeaders(accessToken, baseURL)
	taken := requiredLower(required)
	merged := make(map[string]string, len(headers)+len(required))
	for name, value := range headers {
		if _, ok := taken[strings.ToLower(name)]; !ok {
			merged[name] = value
		}
	}
	maps.Copy(merged, required)
	return merged
}

// ApplyRequiredCodexHeaders adds the identity headers the official Codex endpoint requires
func ApplyRequiredCodexHeaders(headers map[string]string, accessToken, baseURL string) map[string]string {
	return applyRequiredCodexHeaders(headers, accessToken, baseURL)
}

// Lower cased name set of a header map
func requiredLower(h map[string]string) map[string]struct{} {
	out := make(map[string]struct{}, len(h))
	for name := range h {
		out[strings.ToLower(name)] = struct{}{}
	}
	return out
}

// ChatGPT account id the claims of a Codex access token name, empty when none
func codexTokenAccountID(accessToken string) string {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return ""
	}
	auth, _ := DecodeJWTClaims(parts[0] + "." + parts[1] + ".x")["https://api.openai.com/auth"].(map[string]any)
	account, _ := auth["chatgpt_account_id"].(string)
	return strings.TrimSpace(account)
}
