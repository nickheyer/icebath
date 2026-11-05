package skills

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python exception class and message a bundle CRUD call raises
type BundleError struct{ Class, Msg string }

func (e *BundleError) Error() string { return e.Msg }

// One bundle file path and its content digest
type bundleSigEntry struct{ path, digest string }

// Bundle file parser seam the scan race tests wrap
var bundleSteps struct {
	load func(s *Scope, path string) (map[string]any, error)
}

func init() { bundleSteps.load = (*Scope).loadBundleFile }

var bundleCache struct {
	sync.Mutex
	bundles SkillCommands
	sig     []bundleSigEntry
	stamped bool
}

// Bundles dir, the ICEBATH_BUNDLES_DIR override or the profile skill-bundles dir
func (s *Scope) BundlesDir() string {
	if override := os.Getenv("ICEBATH_BUNDLES_DIR"); override != "" {
		return expandPath(override)
	}
	return home.BundlesDir(s.Home)
}

// Sorted yaml then yml bundle files of the bundles dir, names folded on Windows like pathlib glob and sort
func (s *Scope) bundleFiles() []string {
	base := s.BundlesDir()
	if !lexistsFollow(base) {
		return nil
	}
	entries, _ := os.ReadDir(base)
	fold := func(n string) string { return n }
	if runtime.GOOS == "windows" {
		fold = strings.ToLower
	}
	var out []string
	for _, ext := range []string{".yaml", ".yml"} {
		var names []string
		for _, e := range entries {
			if strings.HasSuffix(fold(e.Name()), ext) {
				names = append(names, e.Name())
			}
		}
		sort.Slice(names, func(i, j int) bool { return fold(names[i]) < fold(names[j]) })
		for _, n := range names {
			out = append(out, filepath.Join(base, n))
		}
	}
	return out
}

// Sorted path and sha256 digest of each readable bundle file
func bundlesSig(files []string) []bundleSigEntry {
	var out []bundleSigEntry
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		out = append(out, bundleSigEntry{f, hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].digest < out[j].digest
	})
	return out
}

// Reports whether two bundle signatures are equal
func sameBundleSig(a, b []bundleSigEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Path without its last suffix the way pathlib stem names it
func pathStem(p string) string {
	name := filepath.Base(p)
	if i := strings.LastIndex(name, "."); i > 0 {
		return name[:i]
	}
	return name
}

// Parses one bundle file, nil with a warning for an unusable bundle and an error for undecodable text
func (s *Scope) loadBundleFile(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		s.logger().Warn(fmt.Sprintf("Could not read bundle %s: %s", path, pyOSErrorText(err)))
		return nil, nil
	}
	text, err := decodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	data, err := yamlLoad(text)
	if err != nil {
		s.logger().Warn(fmt.Sprintf("Invalid YAML in bundle %s: %v", path, err))
		return nil, nil
	}
	skip := func(reason string) (map[string]any, error) {
		s.logger().Warn(fmt.Sprintf("Bundle %s %s; skipping", path, reason))
		return nil, nil
	}
	m, ok := data.(map[string]any)
	if !ok {
		return skip("is not a mapping")
	}
	nameVal := any(pathStem(path))
	if pyrepr.Truthy(m["name"]) {
		nameVal = m["name"]
	}
	name := pystr.Strip(pyrepr.Str(nameVal))
	if name == "" {
		return skip("has no name")
	}
	rawSkills, isList := m["skills"].([]any)
	if pyrepr.Truthy(m["skills"]) && !isList {
		return skip("has no skills list")
	}
	if len(rawSkills) == 0 {
		return skip("has no skills list")
	}
	var members []any
	for _, item := range rawSkills {
		if v := pystr.Strip(pyrepr.Str(item)); v != "" {
			members = append(members, v)
		}
	}
	if len(members) == 0 {
		return skip("has empty skills list")
	}
	slug := SlugifySkillName(name)
	if slug == "" {
		return skip("yielded empty slug")
	}
	desc := pystr.Strip(pyrepr.Str(orEmpty(m["description"])))
	if desc == "" {
		desc = fmt.Sprintf("Load %d skills as a bundle", len(members))
	}
	return map[string]any{"name": name, "slug": slug, "skills": members, "path": path, "description": desc,
		"instruction": pystr.Strip(pyrepr.Str(orEmpty(m["instruction"])))}, nil
}

// Value when truthy, the empty string otherwise
func orEmpty(v any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return ""
}

// Text of utf-8-sig bytes, the Python UnicodeDecodeError when they are not strict UTF-8
func decodeUTF8Sig(raw []byte) (string, error) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	for i := 0; i < len(raw); {
		r, n := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && n <= 1 {
			return "", pystr.UTF8DecodeError(raw, i)
		}
		i += n
	}
	return string(raw), nil
}

// Rebuilds the slash key to bundle cache, a duplicate slug keeping the alphabetically first file
func (s *Scope) ScanBundles() (SkillCommands, error) {
	files := s.bundleFiles()
	sig := bundlesSig(files)
	out := SkillCommands{}
	for _, f := range files {
		info, err := bundleSteps.load(s, f)
		if err != nil {
			return nil, err
		}
		if info == nil {
			continue
		}
		key := "/" + info["slug"].(string)
		if prev, dup := out[key]; dup {
			s.logger().Warn(fmt.Sprintf("Duplicate bundle slug %s from %s; keeping %s", key, f, prev["path"]))
			continue
		}
		out[key] = info
	}
	bundleCache.Lock()
	bundleCache.bundles, bundleCache.sig, bundleCache.stamped = out, sig, true
	bundleCache.Unlock()
	return out, nil
}

// Current bundle mapping, rescanned only when the bundle files or their content changed
func (s *Scope) GetSkillBundles() (SkillCommands, error) {
	current := bundlesSig(s.bundleFiles())
	bundleCache.Lock()
	cached, sig, stamped := bundleCache.bundles, bundleCache.sig, bundleCache.stamped
	bundleCache.Unlock()
	if len(cached) == 0 || !stamped || !sameBundleSig(sig, current) {
		return s.ScanBundles()
	}
	return cached, nil
}

// Slash key of a typed bundle command with underscores matching hyphens
func (s *Scope) ResolveBundleCommandKey(command string) (string, bool, error) {
	bundles, err := s.GetSkillBundles()
	if err != nil {
		return "", false, err
	}
	key, ok := ResolveSlashKey(command, bundles)
	return key, ok, nil
}

// Rescans bundles and returns the added, removed, unchanged and total diff
func (s *Scope) ReloadBundles() (map[string]any, error) {
	bundleCache.Lock()
	before := CommandSnapshot(bundleCache.bundles)
	bundleCache.Unlock()
	after, err := s.ScanBundles()
	if err != nil {
		return nil, err
	}
	return DiffCommandSnapshots(before, CommandSnapshot(after)), nil
}

// Bundle infos sorted by slug
func (s *Scope) ListBundles() ([]map[string]any, error) {
	bundles, err := s.GetSkillBundles()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(bundles))
	for _, info := range bundles {
		out = append(out, info)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["slug"].(string) < out[j]["slug"].(string) })
	return out, nil
}

// Bundle invocation message with loaded and missing members, false when the bundle is unknown or loads nothing
func (s *Scope) BuildBundleInvocationMessage(ctx context.Context, cmdKey, userInstruction, taskID, platform string) (string, []string, []string, bool, error) {
	bundles, err := s.GetSkillBundles()
	if err != nil {
		return "", nil, nil, false, err
	}
	info, ok := bundles[cmdKey]
	if !ok {
		return "", nil, nil, false, nil
	}
	bundleName := info["name"].(string)
	members := info["skills"].([]any)
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = pystr.Strip(pyrepr.Str(orEmpty(m)))
	}
	loadedNames, missing, disabled, blocks := s.loadSkillBlocks(ctx, ids,
		func(id string) (loadedSkill, bool) {
			payload, dir, name, ok := s.loadSkillPayload(ctx, id, taskID)
			return loadedSkill{payload, dir, name}, ok
		},
		func(string) string { return fmt.Sprintf("[Loaded as part of the \"%s\" skill bundle.]", bundleName) },
		taskID, skillBlockOpts{disabledNames: s.commandDisabledSkillNames(platform)})
	if len(blocks) == 0 {
		return "", nil, nil, false, nil
	}
	instruction, _ := info["instruction"].(string)
	header := scaffoldHeader(fmt.Sprintf("\"%s\" skill bundle", bundleName), loadedNames, []string{"Bundle: " + bundleName}, missing,
		disabled, instruction, userInstruction)
	return strings.Join(append([]string{header}, blocks...), "\n\n"), loadedNames, missing, true, nil
}

// Canonical file path of a bundle name, a ValueError when the name slugs to nothing
func (s *Scope) BundlePathFor(name string) (string, error) {
	slug := SlugifySkillName(name)
	if slug == "" {
		return "", &BundleError{"ValueError", fmt.Sprintf("Bundle name %s normalizes to an empty slug", pyrepr.StrRepr(name))}
	}
	return filepath.Join(s.BundlesDir(), slug+".yaml"), nil
}

// Writes a bundle atomically keeping an existing file's mode and owner, then refreshes the cache
func (s *Scope) SaveBundle(name string, skills []string, description, instruction string, overwrite bool) (string, error) {
	name = pystr.Strip(name)
	if name == "" {
		return "", &BundleError{"ValueError", "Bundle name is required"}
	}
	var cleaned []any
	for _, sk := range skills {
		if v := pystr.Strip(sk); v != "" {
			cleaned = append(cleaned, v)
		}
	}
	if len(cleaned) == 0 {
		return "", &BundleError{"ValueError", "Bundle must reference at least one skill"}
	}
	path, err := s.BundlePathFor(name)
	if err != nil {
		return "", err
	}
	st, statErr := os.Stat(path)
	if statErr == nil && !overwrite {
		return "", &BundleError{"FileExistsError", "Bundle already exists at " + path}
	}
	if _, err := home.Mkdir(filepath.Dir(path)); err != nil {
		return "", err
	}
	payload := map[string]any{"name": name, "skills": cleaned}
	keys := []string{"name", "skills"}
	for _, kv := range [][2]string{{"description", description}, {"instruction", instruction}} {
		if kv[1] != "" {
			payload[kv[0]] = kv[1]
			keys = append(keys, kv[0])
		}
	}
	config.SetMappingKeys(payload, keys)
	text, err := config.SafeDump(payload)
	if err != nil {
		return "", err
	}
	if statErr == nil {
		err = pyfs.AtomicWriteTextMode(path, string(text), ".tmp_", st.Mode().Perm())
	} else {
		err = pyfs.AtomicWriteText(path, string(text), ".tmp_")
	}
	if err != nil {
		return "", err
	}
	if _, err := s.ScanBundles(); err != nil {
		return "", err
	}
	return path, nil
}

// Deletes a bundle by name and refreshes the cache, a FileNotFoundError when absent
func (s *Scope) DeleteBundle(name string) (string, error) {
	path, err := s.BundlePathFor(name)
	if err != nil {
		return "", err
	}
	if !lexistsFollow(path) {
		return "", &BundleError{"FileNotFoundError", "No bundle at " + path}
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	if _, err := s.ScanBundles(); err != nil {
		return "", err
	}
	return path, nil
}

// Bundle info by slug normalized name, false when absent
func (s *Scope) GetBundle(name string) (map[string]any, bool, error) {
	bundles, err := s.GetSkillBundles()
	if err != nil {
		return nil, false, err
	}
	info, ok := bundles["/"+SlugifySkillName(name)]
	return info, ok, nil
}
