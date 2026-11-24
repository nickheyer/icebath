package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	versionutil "github.com/nickheyer/icebath/pkg/utils/version"
)

const (
	feedFetchTimeout = 10 * time.Second
	maxFeedBytes     = 1 * 1024 * 1024
)

// Newer verdict of two version labels, nil when either is not a valid version
func versionIsNewer(latest, current string) *bool {
	l, err := versionutil.Parse(latest)
	if err != nil {
		return nil
	}
	c, err := versionutil.Parse(current)
	if err != nil {
		return nil
	}
	newer := versionutil.Compare(l, c) > 0
	return &newer
}

// Update check outcome of one plugin, nil pointers and empty strings stand for unknown
type CheckResult struct {
	Name            string
	Class           string
	Current         string
	Latest          string
	UpdateAvailable *bool
	NeedsFixing     string
	MinIcebath      string
	Reason          string
}

// JSON form with null for every unknown field
func (r *CheckResult) ToJSON() map[string]any {
	orNil := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	var available any
	if r.UpdateAvailable != nil {
		available = *r.UpdateAvailable
	}
	return map[string]any{
		"name": r.Name, "class": r.Class, "current": orNil(r.Current), "latest": orNil(r.Latest),
		"update_available": available, "needs_fixing": orNil(r.NeedsFixing), "min_icebath": orNil(r.MinIcebath),
		"reason": r.Reason,
	}
}

// One stripped non empty string field of the installed plugin.yaml, empty otherwise
func readManifestField(pluginDir, key string) string {
	manifest := filepath.Join(pluginDir, "plugin.yaml")
	if st, err := os.Stat(manifest); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	raw, err := os.ReadFile(manifest)
	if err != nil || !utf8.Valid(raw) {
		return ""
	}
	doc, err := config.FastSafeLoad(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
	if err != nil {
		return ""
	}
	data, ok := doc.(map[string]any)
	if !ok {
		return ""
	}
	value, _ := data[key].(string)
	return strings.TrimSpace(value)
}

// Checks installed provenance and the saved update url tag without any network access
func checkLocalProvenance(prov Provenance) *CheckResult {
	result := &CheckResult{Name: prov.Name, Class: string(prov.Class)}
	switch prov.Class {
	case ProvenanceManual:
		result.Reason = "no provenance; not auto-updatable"
		return result
	case ProvenanceDrift:
		result.Reason = fmt.Sprintf("provenance drift — sidecar records %s but the dir has no .git; reinstall from the recorded source",
			errchain.Show(prov.Row["source"]))
		return result
	case ProvenanceSelfCloned:
		result.Reason = "self-cloned; run `icebath plugins adopt` first"
		return result
	}
	row := prov.Row
	if jsonTruthy(row["revision"]) {
		result.Current = anyutil.String(row["revision"])
	}
	if pinned, ok := row["pinned"].(bool); ok && pinned {
		no := false
		result.UpdateAvailable = &no
		shown := result.Current
		if len(shown) > 12 {
			shown = shown[:12]
		}
		if shown == "" {
			shown = "sha"
		}
		result.Reason = "pinned @ " + shown
		return result
	}
	var saved any
	if jsonTruthy(row["update_url"]) {
		saved = row["update_url"]
	}
	claimed := readManifestField(prov.Path, "update_url")
	var claimedValue any
	if claimed != "" {
		claimedValue = claimed
	}
	if saved == nil && claimed != "" {
		result.NeedsFixing = fmt.Sprintf("manifest declares update_url %s but no url was saved at install; "+
			"run `icebath plugins trust-update-url` after review", errchain.Show(claimed))
		return result
	}
	if savedText, isStr := saved.(string); saved != nil && (!isStr || claimed == "" || savedText != claimed) {
		result.NeedsFixing = fmt.Sprintf("update_url mismatch: saved %s, manifest declares %s; "+
			"run `icebath plugins trust-update-url` after review", errchain.Show(saved), errchain.Show(claimedValue))
	}
	return result
}

// Feed document with its version, optional floor and notes url, and string artifacts
func parseFeedYml(text string) (map[string]any, error) {
	doc, err := config.FastSafeLoad([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("invalid feed YAML: %v", err)
	}
	data, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("feed must be a YAML mapping")
	}
	version, _ := data["version"].(string)
	if strings.TrimSpace(version) == "" {
		return nil, errors.New("feed missing 'version'")
	}
	out := map[string]any{"version": strings.TrimSpace(version)}
	floor, _ := data["min_icebath"].(string)
	if strings.TrimSpace(floor) == "" {
		floor, _ = data["min_hermes"].(string)
	}
	if strings.TrimSpace(floor) != "" {
		out["min_icebath"] = strings.TrimSpace(floor)
	}
	if notes, _ := data["notes_url"].(string); strings.TrimSpace(notes) != "" {
		out["notes_url"] = strings.TrimSpace(notes)
	}
	if artifacts, ok := data["artifacts"].(map[string]any); ok {
		if git, present := artifacts["git"]; present && git != nil {
			if _, isStr := git.(string); !isStr {
				return nil, errors.New("feed artifacts.git must be a string")
			}
		}
		kept := map[string]any{}
		for key, value := range artifacts {
			if s, isStr := value.(string); isStr {
				kept[key] = s
			}
		}
		out["artifacts"] = kept
	}
	return out, nil
}

// Stripped update url, refused unless it uses https
func httpsUpdateURL(url any) (string, error) {
	text := ""
	if jsonTruthy(url) {
		text = anyutil.String(url)
	}
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(strings.ToLower(text), "https://") {
		return "", fmt.Errorf("update_url must be an https:// URL, got %s", errchain.Show(text))
	}
	return text, nil
}

// Fetches a feed over https only, redirects included, reading at most one MiB as text
// Round tripper feed fetches use, nil for the default transport
var feedTransport http.RoundTripper

var defaultFetch = func(ctx context.Context, url string) (string, error) {
	target, err := httpsUpdateURL(url)
	if err != nil {
		return "", err
	}
	client := &http.Client{Transport: feedTransport, Timeout: feedFetchTimeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if !strings.HasPrefix(strings.ToLower(req.URL.String()), "https://") {
			return errors.New("update_url redirect must use https://")
		}
		if len(via) > 10 {
			return fmt.Errorf("HTTP Error %d: The HTTP server returned a redirect error that would lead to an infinite loop.", req.Response.StatusCode)
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "icebath-agent")
	resp, err := client.Do(req)
	if err != nil {
		var urlErr interface{ Unwrap() error }
		if errors.As(err, &urlErr) && urlErr.Unwrap() != nil && strings.Contains(urlErr.Unwrap().Error(), "redirect must use https") {
			return "", urlErr.Unwrap()
		}
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, strings.TrimPrefix(resp.Status, fmt.Sprintf("%d ", resp.StatusCode)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
	if err != nil {
		return "", err
	}
	return strings.ToValidUTF8(string(data), "\uFFFD"), nil
}
