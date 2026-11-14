package openai

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Base URL of the ChatGPT Codex backend
const CodexBaseURL = "https://chatgpt.com/backend-api/codex"

// Reports whether a base URL is OpenAI's own Codex endpoint and not a proxy in front of one
func IsOfficialCodexURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	path := strings.TrimRight(u.Path, "/")
	return u.Scheme == "https" && u.Hostname() == "chatgpt.com" && (u.Port() == "" || u.Port() == "443") &&
		(path == "/backend-api/codex" || strings.HasPrefix(path, "/backend-api/codex/"))
}

// Workspace headers the Codex backend derives from the OAuth token, none for a token that cannot be read
func CodexAccountHeaders(accessToken string) http.Header {
	h := http.Header{}
	parts := strings.Split(accessToken, ".")
	if strip(accessToken) == "" || len(parts) < 2 {
		return h
	}
	payload, err := base64.URLEncoding.DecodeString(parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4))
	if err != nil {
		return h
	}
	claims, err := wire.ParseValue(payload)
	if err != nil {
		return h
	}
	auth := claims.Get("https://api.openai.com/auth")
	if account := auth.Get("chatgpt_account_id").Text(); account != "" {
		h["ChatGPT-Account-ID"] = []string{account}
	}
	residency := auth.Get("chatgpt_data_residency")
	if !residency.Truthy() {
		residency = auth.Get("chatgpt_compute_residency")
	}
	if region := strip(residency.Text()); region != "" {
		h["x-openai-internal-codex-residency"] = []string{region}
	}
	return h
}

// Identity and account headers for a Codex endpoint, the product's own identity only on the official one
func CodexHeaders(accessToken, baseURL, version string) http.Header {
	h := CodexAccountHeaders(accessToken)
	if IsOfficialCodexURL(baseURL) {
		h.Set("User-Agent", "icebath/"+version)
		h["originator"] = []string{"icebath"}
		return h
	}
	h.Set("User-Agent", "codex_cli_rs/0.0.0 (icebath)")
	h["originator"] = []string{"codex_cli_rs"}
	return h
}

// Puts the required Codex identity back over user and provider header overrides on the official endpoint
func RequireCodexHeaders(h http.Header, accessToken, baseURL, version string) {
	if !IsOfficialCodexURL(baseURL) {
		return
	}
	required := CodexHeaders(accessToken, baseURL, version)
	for name := range h {
		for want := range required {
			if strings.EqualFold(name, want) {
				delete(h, name)
			}
		}
	}
	for name, values := range required {
		h[name] = values
	}
}

// Compresses the body of a Responses call to the official Codex endpoint with zstd, as its own client does
func CompressCodexCall(call *wire.Call, baseURL string) error {
	if !IsOfficialCodexURL(baseURL) || call.Method != http.MethodPost || !strings.HasSuffix(call.Path, "/responses") ||
		call.Header.Get("Content-Encoding") != "" {
		return nil
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return err
	}
	call.Body = enc.EncodeAll(call.Body, nil)
	call.Header.Set("Content-Encoding", "zstd")
	return enc.Close()
}
