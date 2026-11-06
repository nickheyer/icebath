package profiles

import (
	"regexp"
	"strings"
	"unicode"
)

// Home relative credential directories of an OS account plus direnv files
var osCredentialStores = []string{".ssh", ".aws", ".gnupg", ".kube", ".docker", ".azure", ".config/gh", ".config/gcloud", ".envrc"}

// Secret value caches and the sidecar token, at the home top and where earlier releases kept them
var secretCacheStores = []string{
	"bws_cache.json", "bws_cache.enc.json", "op_cache.json", "photon-sidecar.json",
	"cache/bws_cache.json", "cache/bws_cache.enc.json", "cache/op_cache.json", "runtime/photon-sidecar.json",
}

// Home relative secret cache stores of home_data_layout, a fresh copy per call
func SecretCacheStores() []string { return append([]string(nil), secretCacheStores...) }

// Profile relative stores holding credentials, never cloned, exported or shared
var ProfileCredentialPaths = func() map[string]bool {
	paths := []string{
		"auth.json", ".env", "auth/google_oauth.json", ".op.env", "npmrc", ".anthropic_oauth.json",
		"google_token.json", "google_oauth_pending.json", "google_client_secret.json",
		"google_chat_user_tokens", "google_chat_user_client_secret.json", "google_chat_user_oauth_pending.json",
		"google_chat_user_token.json", "google_chat_user_oauth_pending",
		"slack_tokens.json", "honcho.json", "mem0.json", "webhook_subscriptions.json", "teams_pipeline_store.json",
		"mcp-tokens", "vault", "browser-profile", "browser_auth", "bot-desktop", "webapp",
		"browser-profiles", "browser_profiles", "pairing", "platforms/pairing", "feishu_comment_pairing.json",
		"whatsapp/session", "platforms/whatsapp/session", "matrix/store", "platforms/matrix/store",
		"workspace/meetings/node_token.json", "weixin/accounts", ".copilot_jwt.json", "proxy", "chrome-debug",
		"home", "backups", "state-snapshots",
	}
	paths = append(paths, secretCacheStores...)
	paths = append(paths, osCredentialStores...)
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	return set
}()

// Case folded part tuples of every credential store
var credentialPathParts = func() [][]string {
	out := make([][]string, 0, len(ProfileCredentialPaths))
	for p := range ProfileCredentialPaths {
		out = append(out, strings.Split(casefold(p), "/"))
	}
	return out
}()

var storeCopyRe = regexp.MustCompile(`^(?:auth\.json\..+|\.env\.bak.*` +
	`|config\.yaml\.(?:bak-\d{8}t\d{6}z(?:\.\d+)?|bak\.\d+|corrupt\..*|bak-pre-migrate-.*))$`)

// Full case folding of characters whose Python casefold differs from Go lowercasing
var fullFolds = map[rune]string{
	'ß': "ss", 'ẞ': "ss", 'İ': "i̇", 'ﬀ': "ff", 'ﬁ': "fi", 'ﬂ': "fl", 'ﬃ': "ffi", 'ﬄ': "ffl", 'ﬅ': "st", 'ﬆ': "st",
	'ſ': "s", 'µ': "μ", 'ς': "σ", 'ϐ': "β", 'ϑ': "θ", 'ϕ': "φ", 'ϖ': "π", 'ϰ': "κ", 'ϱ': "ρ", 'ϵ': "ε", 'ẛ': "ṡ", 'ι': "ι",
}

// Python str.casefold of s for every fold that can reach an ASCII store name
func casefold(s string) string {
	var b strings.Builder
	for _, r := range s {
		if f, ok := fullFolds[r]; ok {
			b.WriteString(f)
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// Case folded copy of path parts
func foldParts(parts []string) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = casefold(p)
	}
	return out
}

// Reports whether a matches the first len(a) parts of b
func partsPrefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Reports whether parts name a credential store, anything below one, or a root copy of one
func ProfilePathIsPrivate(parts []string) bool {
	folded := foldParts(parts)
	if len(folded) == 1 && storeCopyRe.MatchString(folded[0]) {
		return true
	}
	for _, store := range credentialPathParts {
		if partsPrefix(store, folded) {
			return true
		}
	}
	return false
}

// Reports whether parts name a strict ancestor of a credential store
func ProfilePathContainsPrivateStore(parts []string) bool {
	folded := foldParts(parts)
	for _, store := range credentialPathParts {
		if len(store) > len(folded) && partsPrefix(folded, store) {
			return true
		}
	}
	return false
}
