package gatewayrun

import (
	_ "embed"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Built in generic status phrases shipped beside the gateway
//
//go:embed assets/status_phrases.yaml
var builtinStatusPhrasesYAML []byte

// UI surfaces a status phrase catalog carries, long running status and generic
var statusSurfaces = []string{"status", "generic"}

const (
	maxCustomPhrasesPerSurface = 80
	maxPhraseChars             = 160
)

// Profile relative files read before any configured catalog
var conventionalPhrasePaths = []string{"status_phrases.yaml", "status_phrases"}

// Display keys naming a phrase section, the legacy alias first
var statusPhraseConfigKeys = []string{"generic_status_phrases", "status_phrases"}

// Status phrase lists per surface
type statusPhraseCatalog map[string][]string

// Phrases used when no catalog loads
var fallbackStatusPhrases = statusPhraseCatalog{
	"status":  {"still on it", "still working through it", "waiting for the result"},
	"generic": {"on it", "one sec", "checking that now"},
}

// Fallback phrases replaced by the shipped catalog
var defaultStatusPhrases = func() statusPhraseCatalog {
	catalog := copyPhraseCatalog(fallbackStatusPhrases)
	if loaded, err := config.SafeLoad(builtinStatusPhrasesYAML); err == nil {
		if mapping, ok := loaded.(map[string]any); ok {
			mergePhraseMapping(catalog, mapping, "replace")
		}
	}
	return catalog
}()

// Distinct non empty phrases of a list within the per surface and per phrase limits
func cleanPhraseList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	if len(items) > maxCustomPhrasesPerSurface {
		items = items[:maxCustomPhrasesPerSurface]
	}
	cleaned := []string{}
	for _, item := range items {
		if !pyrepr.Truthy(item) {
			item = ""
		}
		phrase := pystr.Strip(pyrepr.Str(item))
		if phrase != "" && len([]rune(phrase)) <= maxPhraseChars && !slices.Contains(cleaned, phrase) {
			cleaned = append(cleaned, phrase)
		}
	}
	return cleaned
}

// Merges one section's phrases, replacing a surface in replace mode and appending otherwise
func mergePhraseMapping(catalog statusPhraseCatalog, section map[string]any, inheritedMode string) {
	mode := section["mode"]
	if !pyrepr.Truthy(mode) {
		mode = inheritedMode
		if inheritedMode == "" {
			mode = "append"
		}
	}
	replace := strings.ToLower(pystr.Strip(pyrepr.Str(mode))) == "replace"
	phraseMap, isMap := section["phrases"].(map[string]any)
	if !isMap {
		phraseMap = section
	}
	for _, surface := range statusSurfaces {
		phrases := cleanPhraseList(phraseMap[surface])
		if len(phrases) == 0 {
			continue
		}
		if replace {
			catalog[surface] = phrases
		} else {
			catalog[surface] = append(slices.Clone(catalog[surface]), phrases...)
		}
	}
}

// Merges one YAML phrase file, a file that cannot be read or parsed adding nothing
func mergePhraseFile(catalog statusPhraseCatalog, path, inheritedMode string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	loaded, err := config.SafeLoad(data)
	if err != nil {
		return
	}
	if mapping, ok := loaded.(map[string]any); ok {
		mergePhraseMapping(catalog, mapping, inheritedMode)
	}
}

// Whether a file name carries a YAML suffix
func hasYAMLSuffix(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

// YAML files under the base dir at a relative path, none for absolute, parent or escaping paths
func iterPhraseFiles(baseDir string, rawPath any) []string {
	if !pyrepr.Truthy(rawPath) {
		rawPath = ""
	}
	raw := pystr.Strip(pyrepr.Str(rawPath))
	candidate := pypath.Expanduser(raw)
	if raw == "" || filepath.IsAbs(candidate) || slices.Contains(strings.Split(filepath.ToSlash(candidate), "/"), "..") {
		return nil
	}
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	path := filepath.Join(base, candidate)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if rel, err := filepath.Rel(base, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if info.Mode().IsRegular() && hasYAMLSuffix(path) {
		return []string{path}
	}
	if !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	files := []string{}
	for _, entry := range entries {
		child := filepath.Join(path, entry.Name())
		if st, err := os.Stat(child); err == nil && st.Mode().IsRegular() && hasYAMLSuffix(child) {
			files = append(files, child)
		}
	}
	sort.Strings(files)
	return files
}

// Merges every phrase file one path or a list of paths names
func mergePhrasePaths(catalog statusPhraseCatalog, paths any, baseDir, inheritedMode string) {
	if paths == nil {
		return
	}
	list, ok := paths.([]any)
	if !ok {
		list = []any{paths}
	}
	for _, rawPath := range list {
		for _, file := range iterPhraseFiles(baseDir, rawPath) {
			mergePhraseFile(catalog, file, inheritedMode)
		}
	}
}

func copyPhraseCatalog(catalog statusPhraseCatalog) statusPhraseCatalog {
	out := statusPhraseCatalog{}
	for surface, phrases := range catalog {
		out[surface] = slices.Clone(phrases)
	}
	return out
}

// Merges one display status phrase section, its files first and then its inline phrases
func mergePhraseConfig(catalog statusPhraseCatalog, section any, baseDir string) {
	mapping, ok := section.(map[string]any)
	if !ok {
		return
	}
	mode := mapping["mode"]
	if !pyrepr.Truthy(mode) {
		mode = "append"
	}
	modeText := strings.ToLower(pystr.Strip(pyrepr.Str(mode)))
	for _, key := range []string{"path", "paths"} {
		mergePhrasePaths(catalog, mapping[key], baseDir, modeText)
	}
	mergePhraseMapping(catalog, mapping, "")
}

// Built in phrases plus the profile files and the global then per platform display sections
func resolveStatusPhraseCatalog(home string, userConfig map[string]any, platformKey string) statusPhraseCatalog {
	catalog := copyPhraseCatalog(defaultStatusPhrases)
	conventional := make([]any, len(conventionalPhrasePaths))
	for i, p := range conventionalPhrasePaths {
		conventional[i] = p
	}
	mergePhrasePaths(catalog, conventional, home, "")
	display, ok := userConfig["display"].(map[string]any)
	if !ok {
		return catalog
	}
	sections := []map[string]any{display}
	if platforms, ok := display["platforms"].(map[string]any); ok && platformKey != "" {
		if section, ok := platforms[platformKey].(map[string]any); ok {
			sections = append(sections, section)
		}
	}
	for _, section := range sections {
		for _, key := range statusPhraseConfigKeys {
			mergePhraseConfig(catalog, section[key], home)
		}
	}
	return catalog
}

// UI surface bucket of an internal gateway event
func classifyStatusContext(kind string) string {
	switch strings.ToLower(pystr.Strip(kind)) {
	case "heartbeat", "waiting", "long_running", "status":
		return "status"
	}
	return "generic"
}

// Picks a short generic status phrase avoiding the recent ones, never embedding tool text
func chooseStatusPhrase(kind string, recent *[]string, pick func(n int) int, catalog statusPhraseCatalog) string {
	if len(catalog) == 0 {
		catalog = defaultStatusPhrases
	}
	candidates := slices.Clone(catalog[classifyStatusContext(kind)])
	if len(candidates) == 0 {
		candidates = slices.Clone(catalog["generic"])
	}
	if len(candidates) == 0 {
		candidates = slices.Clone(defaultStatusPhrases["generic"])
	}
	if recent != nil && len(*recent) > 0 {
		fresh := []string{}
		for _, p := range candidates {
			if !slices.Contains(*recent, p) {
				fresh = append(fresh, p)
			}
		}
		if len(fresh) > 0 {
			candidates = fresh
		}
	}
	if pick == nil {
		pick = rand.IntN
	}
	phrase := candidates[pick(len(candidates))]
	if recent != nil {
		*recent = append(*recent, phrase)
		if len(*recent) > 6 {
			*recent = slices.Clone((*recent)[len(*recent)-6:])
		}
	}
	return phrase
}
