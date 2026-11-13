package trajectorycompressor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gomlx/go-huggingface/hub"

	"github.com/nickheyer/icebath/pkg/tokenizers"
)

// Encoder gives the token ids of a text
type Encoder = tokenizers.Encoder

// Whether an environment value switches a Hugging Face hub option on
func hfTrue(v string) bool {
	switch strings.ToUpper(v) {
	case "1", "ON", "YES", "TRUE":
		return true
	}
	return false
}

// The value of a variable, the default only when the variable is unset
func getenv(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// The first of two variables that holds a value
func envOr(a, b string) string {
	if v := os.Getenv(a); v != "" {
		return v
	}
	return os.Getenv(b)
}

// A path with a leading tilde and environment variables resolved
func expandPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && (p == "~" || strings.HasPrefix(p, "~/")) {
		p = home + p[1:]
	}
	return os.ExpandEnv(p)
}

// Whether the Hugging Face staging hub replaces the public one
func hfStaging() bool { return hfTrue(os.Getenv("HUGGINGFACE_CO_STAGING")) }

// HF_HOME, else the huggingface folder under XDG_CACHE_HOME or the home cache
func hfHome() string {
	return expandPath(getenv("HF_HOME", filepath.Join(getenv("XDG_CACHE_HOME", "~/.cache"), "huggingface")))
}

// The hub cache folder, HF_HUB_CACHE, then HUGGINGFACE_HUB_CACHE, then the hub folder of the Hugging Face home
func hfHubCache() string {
	return expandPath(getenv("HF_HUB_CACHE", getenv("HUGGINGFACE_HUB_CACHE", filepath.Join(hfHome(), "hub"))))
}

// Hub endpoint, HF_ENDPOINT without trailing slashes unless staging mode selects the staging hub
func hfEndpoint() string {
	if hfStaging() {
		return "https://hub-ci.huggingface.co"
	}
	return strings.TrimRight(getenv("HF_ENDPOINT", "https://huggingface.co"), "/")
}

// Whether the hub is offline, HF_HUB_OFFLINE or else TRANSFORMERS_OFFLINE
func hfOffline() bool { return hfTrue(envOr("HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE")) }

// Token text with carriage returns and newlines removed and spaces stripped
func cleanToken(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", ""))
}

// Access token from HF_TOKEN or HUGGING_FACE_HUB_TOKEN, then the token file
func hfToken() string {
	if hfTrue(os.Getenv("HF_HUB_DISABLE_IMPLICIT_TOKEN")) {
		return ""
	}
	if v := cleanToken(envOr("HF_TOKEN", "HUGGING_FACE_HUB_TOKEN")); v != "" {
		return v
	}
	path := expandPath(getenv("HF_TOKEN_PATH", filepath.Join(hfHome(), "token")))
	if hfStaging() {
		path = expandPath("~/.cache/huggingface_staging/token")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return cleanToken(string(data))
}

// Snapshot folder of the cached main revision of a model repo, empty when it was never downloaded
func cachedSnapshot(cacheDir, repoID string) string {
	folder := filepath.Join(cacheDir, "models--"+strings.ReplaceAll(repoID, "/", "--"))
	ref, err := os.ReadFile(filepath.Join(folder, "refs", "main"))
	if err != nil {
		return ""
	}
	snap := filepath.Join(folder, "snapshots", strings.TrimSpace(string(ref)))
	if st, err := os.Stat(snap); err == nil && st.IsDir() {
		return snap
	}
	return ""
}

// Tokenizer files of a local folder or a hub model, offline mode reading the cached snapshot only
func tokenizerSource(name string) (tokenizers.Source, error) {
	if st, err := os.Stat(name); err == nil && st.IsDir() {
		return tokenizers.Dir(name), nil
	}
	cache := hfHubCache()
	if hfOffline() {
		snap := cachedSnapshot(cache, name)
		if snap == "" {
			return nil, fmt.Errorf("the hub is offline and %s has no cached snapshot under %s", name, cache)
		}
		return tokenizers.Dir(snap), nil
	}
	if os.Getenv("HF_OIDC_RESOURCE") != "" {
		return nil, fmt.Errorf("the access token of %s comes from an OIDC exchange, set HF_TOKEN", name)
	}
	repo := hub.New(name).WithCacheDir(cache).WithAuth(hfToken()).WithEndpoint(hfEndpoint()).WithProgressBar(false)
	if err := repo.DownloadInfo(false); err != nil {
		return nil, err
	}
	return hubSource{repo}, nil
}

// hubSource reads the files of a hub model, downloading each into the hub cache on first use
type hubSource struct{ repo *hub.Repo }

func (s hubSource) Has(name string) bool { return s.repo.HasFile(name) }

func (s hubSource) Read(name string) ([]byte, error) {
	path, err := s.repo.DownloadFile(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// LoadTokenizer loads the tokenizer of a local folder or a hub model from the tokenizer files it ships
func LoadTokenizer(name string) (Encoder, error) {
	src, err := tokenizerSource(name)
	if err != nil {
		return nil, err
	}
	return tokenizers.Load(src)
}
