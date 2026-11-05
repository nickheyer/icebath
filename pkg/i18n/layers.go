package i18n

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/nickheyer/icebath/pkg/home"
)

const CoreSurface = "core"

var Surfaces = []string{CoreSurface, "tui", "desktop"}

var (
	languageIDRe = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9]{2,8})*$`)
	localeFileRe = regexp.MustCompile(`^([a-z]{2,3}(?:-[a-z0-9]{2,8})*)(?:\.([a-z]+))?\.ya?ml$`)
)

// NormalizeLanguageID lowercases, trims and swaps underscores for hyphens, empty for non text
func NormalizeLanguageID(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "_", "-")
}

// IsLanguageID reports whether the normalized value is a language subtag with optional parts
func IsLanguageID(value any) bool {
	return languageIDRe.MatchString(NormalizeLanguageID(value))
}

func surfaceKnown(surface string) bool {
	for _, s := range Surfaces {
		if s == surface {
			return true
		}
	}
	return false
}

// Flatten turns a nested mapping into dotted keys keeping only text leaves
func Flatten(node any, prefix string, out map[string]string) map[string]string {
	if out == nil {
		out = map[string]string{}
	}
	join := func(k any) string {
		if prefix == "" {
			return pyrepr.Str(k)
		}
		return prefix + "." + pyrepr.Str(k)
	}
	switch n := node.(type) {
	case pyMap:
		for _, p := range n {
			Flatten(p.Value, join(p.Key), out)
		}
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			Flatten(n[k], join(k), out)
		}
	case map[string]string:
		for k, v := range n {
			out[join(k)] = v
		}
	case string:
		out[prefix] = n
	}
	return out
}

// NonTextLeaves lists dotted paths of leaves that are neither text nor a mapping
func NonTextLeaves(node any, prefix string) []string {
	join := func(k any) string {
		if prefix == "" {
			return pyrepr.Str(k)
		}
		return prefix + "." + pyrepr.Str(k)
	}
	switch n := node.(type) {
	case pyMap:
		found := []string{}
		for _, p := range n {
			found = append(found, NonTextLeaves(p.Value, join(p.Key))...)
		}
		return found
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		found := []string{}
		for _, k := range keys {
			found = append(found, NonTextLeaves(n[k], join(k))...)
		}
		return found
	case map[string]string:
		return []string{}
	case string:
		return []string{}
	}
	if prefix == "" {
		return []string{"<root>"}
	}
	return []string{prefix}
}

// ParseLocaleYAML parses locale YAML bytes into a flat catalog, failing on a non mapping document
func ParseLocaleYAML(name string, data []byte) (map[string]string, error) {
	doc, err := loadYAML11(data)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return map[string]string{}, nil
	}
	if _, ok := doc.(pyMap); !ok {
		return nil, fmt.Errorf("%s: top level must be a mapping, got %s", name, pyrepr.TypeName(doc))
	}
	return Flatten(doc, "", nil), nil
}

// ParseLocaleFile parses one locale YAML file into a flat catalog
func ParseLocaleFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseLocaleYAML(path, data)
}

// Parses a locale file out of a file system, used for the bundled layer
func parseLocaleFS(fsys fs.FS, name, label string) (map[string]string, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	return ParseLocaleYAML(label, data)
}

// LoadLocaleSource flattens a mapping source or parses a path source
func LoadLocaleSource(source any) (map[string]string, error) {
	switch s := source.(type) {
	case map[string]any, map[string]string, pyMap:
		return Flatten(s, "", nil), nil
	case string:
		return ParseLocaleFile(s)
	}
	return nil, fmt.Errorf("TypeError: locale source must be a mapping or a path to a YAML file, got %s", pyrepr.TypeName(source))
}

// LocaleFile is one locale YAML found in a directory
type LocaleFile struct {
	Lang    string
	Surface string
	Path    string
}

// ScanLocaleDir lists every lang and surface YAML in a directory sorted by name, logging unknown surfaces
func ScanLocaleDir(directory string) []LocaleFile {
	return scanLocaleFS(os.DirFS(directory), directory)
}

func scanLocaleFS(fsys fs.FS, label string) []LocaleFile {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return []LocaleFile{}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	found := []LocaleFile{}
	for _, e := range entries {
		m := localeFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		info, err := fs.Stat(fsys, e.Name())
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		surface := m[2]
		if surface == "" {
			surface = CoreSurface
		}
		path := filepath.Join(label, e.Name())
		if !surfaceKnown(surface) {
			slog.Warn(fmt.Sprintf("Ignoring locale file %s: unknown surface %s (expected one of %s)", path, pyrepr.StrRepr(surface), strings.Join(Surfaces, ", ")))
			continue
		}
		found = append(found, LocaleFile{Lang: m[1], Surface: surface, Path: path})
	}
	return found
}

// PackEntry is one registered language pack layer, later registrations winning
type PackEntry struct {
	Lang     string
	Surface  string
	Messages map[string]string
	Source   string
	Endonym  string
	RTL      bool
	token    int
}

// PackInfo is the newest pack metadata for a language
type PackInfo struct {
	Endonym string
	RTL     bool
	Source  string
}

type langSurface struct{ lang, surface string }

var (
	layerMu          sync.Mutex
	packTokens       int
	packs            []*PackEntry
	packCache        = map[langSurface]map[string]string{}
	overlayCache     = map[[3]string]map[string]string{}
	overlayFileCache = map[string]map[langSurface]bool{}
)

// RegisterPack adds a pack layer for a language and surface and resets the facade caches
func RegisterPack(lang, surface string, messages map[string]string, source, endonym string, rtl bool) (*PackEntry, error) {
	langID := NormalizeLanguageID(lang)
	if !IsLanguageID(langID) {
		return nil, fmt.Errorf("invalid language id %s (expected e.g. 'pl', 'pt-br', 'zh-hant')", pyrepr.StrRepr(lang))
	}
	if !surfaceKnown(surface) {
		return nil, fmt.Errorf("unknown locale surface %s (expected one of %s)", pyrepr.StrRepr(surface), strings.Join(Surfaces, ", "))
	}
	copied := make(map[string]string, len(messages))
	for k, v := range messages {
		copied[k] = v
	}
	layerMu.Lock()
	packTokens++
	entry := &PackEntry{Lang: langID, Surface: surface, Messages: copied, Source: source, Endonym: endonym, RTL: rtl, token: packTokens}
	packs = append(packs, entry)
	layerMu.Unlock()
	ResetLanguageCache()
	slog.Debug(fmt.Sprintf("i18n pack registered: %s/%s from %s (%d keys)", langID, surface, source, len(copied)))
	return entry, nil
}

// UnregisterPack drops one registration, false when it was already gone
func UnregisterPack(entry *PackEntry) bool {
	layerMu.Lock()
	idx := -1
	for i, e := range packs {
		if e == entry {
			idx = i
			break
		}
	}
	if idx < 0 {
		layerMu.Unlock()
		return false
	}
	packs = append(packs[:idx:idx], packs[idx+1:]...)
	layerMu.Unlock()
	ResetLanguageCache()
	return true
}

// PackLayer merges pack messages for a language and surface in registration order
func PackLayer(lang, surface string) map[string]string {
	key := langSurface{lang, surface}
	layerMu.Lock()
	defer layerMu.Unlock()
	if cached, ok := packCache[key]; ok {
		return cached
	}
	merged := map[string]string{}
	for _, e := range packs {
		if e.Lang == lang && e.Surface == surface {
			for k, v := range e.Messages {
				merged[k] = v
			}
		}
	}
	packCache[key] = merged
	return merged
}

func packSurfaces() map[langSurface]bool {
	layerMu.Lock()
	defer layerMu.Unlock()
	out := map[langSurface]bool{}
	for _, e := range packs {
		out[langSurface{e.Lang, e.Surface}] = true
	}
	return out
}

// PackSurfaces lists every language and surface pair a pack supplies
func PackSurfaces() [][2]string {
	return sortedPairs(packSurfaces())
}

// PackLanguages lists the languages some pack supplies, sorted
func PackLanguages() []string {
	return langsOf(packSurfaces())
}

// PackInfoFor reports the newest pack metadata for a language, nil when no pack has it
func PackInfoFor(lang string) *PackInfo {
	layerMu.Lock()
	var entries []*PackEntry
	for _, e := range packs {
		if e.Lang == lang {
			entries = append(entries, e)
		}
	}
	layerMu.Unlock()
	if len(entries) == 0 {
		return nil
	}
	info := &PackInfo{RTL: entries[len(entries)-1].RTL, Source: entries[len(entries)-1].Source}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Endonym != "" {
			info.Endonym = entries[i].Endonym
			info.RTL = entries[i].RTL
			break
		}
	}
	return info
}

// OverlayDir is the user overlay directory of a home
func OverlayDir(h string) string { return home.LocalesDir(h) }

// OverlayLayer reads the home overlay for a language and surface, empty and logged when missing or broken
func OverlayLayer(h, lang, surface string) map[string]string {
	key := [3]string{h, lang, surface}
	layerMu.Lock()
	if cached, ok := overlayCache[key]; ok {
		layerMu.Unlock()
		return cached
	}
	layerMu.Unlock()
	name := lang + ".yaml"
	if surface != CoreSurface {
		name = lang + "." + surface + ".yaml"
	}
	path := filepath.Join(OverlayDir(h), name)
	flat := map[string]string{}
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		parsed, err := ParseLocaleFile(path)
		if err != nil {
			slog.Warn(fmt.Sprintf("Failed to load i18n overlay %s: %v", path, err))
		} else {
			flat = parsed
		}
	}
	layerMu.Lock()
	overlayCache[key] = flat
	layerMu.Unlock()
	return flat
}

func overlaySurfaces(h string) map[langSurface]bool {
	layerMu.Lock()
	if cached, ok := overlayFileCache[h]; ok {
		layerMu.Unlock()
		return cached
	}
	layerMu.Unlock()
	found := map[langSurface]bool{}
	dir := OverlayDir(h)
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		for _, f := range ScanLocaleDir(dir) {
			found[langSurface{f.Lang, f.Surface}] = true
		}
	}
	layerMu.Lock()
	overlayFileCache[h] = found
	layerMu.Unlock()
	return found
}

// OverlaySurfaces lists every language and surface pair with an overlay file under the home
func OverlaySurfaces(h string) [][2]string { return sortedPairs(overlaySurfaces(h)) }

// OverlayLanguages lists languages with at least one overlay file under the home
func OverlayLanguages(h string) []string { return langsOf(overlaySurfaces(h)) }

// LayerSurfaceCatalog merges overlay and packs for one surface, packs winning, bundled excluded
func LayerSurfaceCatalog(h, lang, surface string) map[string]string {
	merged := map[string]string{}
	for k, v := range OverlayLayer(h, lang, surface) {
		merged[k] = v
	}
	for k, v := range PackLayer(lang, surface) {
		merged[k] = v
	}
	return merged
}

// LayeredLanguages lists languages supplied by the home overlay or any pack
func LayeredLanguages(h string) []string {
	set := map[string]bool{}
	for _, l := range OverlayLanguages(h) {
		set[l] = true
	}
	for _, l := range PackLanguages() {
		set[l] = true
	}
	return sortedKeys(set)
}

// ClearLayerCache drops merged layer views but keeps the registrations
func ClearLayerCache() {
	layerMu.Lock()
	defer layerMu.Unlock()
	packCache = map[langSurface]map[string]string{}
	overlayCache = map[[3]string]map[string]string{}
	overlayFileCache = map[string]map[langSurface]bool{}
}

// ResetRegistryForTests drops every pack registration and cached view
func ResetRegistryForTests() {
	layerMu.Lock()
	packs = nil
	layerMu.Unlock()
	ClearLayerCache()
}

// RegisteredPacks returns the registrations in order
func RegisteredPacks() []*PackEntry {
	layerMu.Lock()
	defer layerMu.Unlock()
	return append([]*PackEntry{}, packs...)
}

func sortedPairs(set map[langSurface]bool) [][2]string {
	out := make([][2]string, 0, len(set))
	for k := range set {
		out = append(out, [2]string{k.lang, k.surface})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}

func langsOf(set map[langSurface]bool) []string {
	langs := map[string]bool{}
	for k := range set {
		langs[k.lang] = true
	}
	return sortedKeys(langs)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
