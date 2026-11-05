package skills

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// OSV query endpoint, request timeout, verdict lifetime and cache bounds
var (
	osvEndpoint      = envOr("OSV_ENDPOINT", "https://api.osv.dev/v1/query")
	osvTimeout       = 10 * time.Second
	osvCacheTTL      = osvTTLFromEnv()
	osvCacheMax      = 256
	osvDiskCacheVers = 1.0
)

// Process env value or a default
func envOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// Verdict lifetime in seconds from OSV_CHECK_CACHE_TTL, an hour by default
func osvTTLFromEnv() float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(envOr("OSV_CHECK_CACHE_TTL", "3600")), 64)
	if err != nil {
		return 3600
	}
	return f
}

// One cached verdict with its expiry in unix seconds, the result nil for a clean package
type osvEntry struct {
	expiry float64
	result *string
}

var (
	osvCacheMu     sync.Mutex
	osvCache       = map[[3]string]osvEntry{}
	osvDiskLoaded  bool
	osvNow         = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	osvQueryFn     = queryOSV
	osvHomeResolve = home.Process
)

// Debug log line of the OSV check
func slogDebug(msg string) { slog.Default().Debug(msg, "logger", "tools.osv_check") }

// Persistent verdict cache path under the active home, created on demand
func osvDiskCachePath() string {
	h := osvHomeResolve()
	if h == "" {
		return ""
	}
	dir := filepath.Join(h, "cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return filepath.Join(dir, "osv_check.json")
}

// Loads unexpired disk verdicts once per process, retrying after a transient read error
func loadOSVDiskCache() {
	if osvDiskLoaded {
		return
	}
	path := osvDiskCachePath()
	if path == "" {
		osvDiskLoaded = true
		return
	}
	raw, err := os.ReadFile(path)
	var data map[string]any
	if err == nil {
		if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xEF\xBB\xBF")), &data) != nil {
			data = nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return
	}
	osvDiskLoaded = true
	if data == nil || data["version"] != osvDiskCacheVers {
		return
	}
	entries, _ := data["entries"].(map[string]any)
	now := osvNow()
	for keyStr, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		expiry, ok := entry["expiry"].(float64)
		if !ok || expiry <= now {
			continue
		}
		parts := strings.SplitN(keyStr, "|", 3)
		if len(parts) != 3 {
			continue
		}
		key := [3]string{parts[0], parts[1], parts[2]}
		if _, ok := osvCache[key]; ok {
			continue
		}
		var result *string
		if s, ok := entry["result"].(string); ok {
			result = &s
		}
		osvCache[key] = osvEntry{expiry, result}
	}
}

// Writes the in process cache to disk atomically, the caller holding the cache lock
func saveOSVDiskCache() {
	path := osvDiskCachePath()
	if path == "" {
		return
	}
	entries := map[string]any{}
	for k, e := range osvCache {
		var result any
		if e.result != nil {
			result = *e.result
		}
		entries[strings.Join(k[:], "|")] = map[string]any{"expiry": e.expiry, "result": result}
	}
	data, err := json.Marshal(map[string]any{"version": 1, "entries": entries})
	if err == nil {
		err = config.AtomicWrite(path, data, 0o644)
	}
	if err != nil {
		slogDebug(fmt.Sprintf("Failed to save OSV disk cache to %s: %v", path, err))
	}
}

// Fresh cached verdict for a key
func osvCacheGet(key [3]string) (bool, *string) {
	osvCacheMu.Lock()
	defer osvCacheMu.Unlock()
	loadOSVDiskCache()
	if e, ok := osvCache[key]; ok && osvNow() < e.expiry {
		return true, e.result
	}
	delete(osvCache, key)
	return false, nil
}

// Stores a verdict, pruning expired entries and resetting a full cache
func osvCachePut(key [3]string, result *string) {
	osvCacheMu.Lock()
	defer osvCacheMu.Unlock()
	loadOSVDiskCache()
	if len(osvCache) >= osvCacheMax {
		now := osvNow()
		for k, e := range osvCache {
			if e.expiry <= now {
				delete(osvCache, k)
			}
		}
		if len(osvCache) >= osvCacheMax {
			osvCache = map[[3]string]osvEntry{}
		}
	}
	osvCache[key] = osvEntry{osvNow() + osvCacheTTL, result}
	saveOSVDiskCache()
}

// Blocked message when an npx, uvx or pipx package has MAL advisories, empty when clean or unchecked
func CheckPackageForMalware(command string, args []string) (string, error) {
	ecosystem := inferEcosystem(command)
	if ecosystem == "" {
		return "", nil
	}
	pkg, version := parsePackageFromArgs(args, ecosystem)
	if pkg == "" {
		return "", nil
	}
	key := [3]string{ecosystem, pkg, version}
	if hit, cached := osvCacheGet(key); hit {
		if cached == nil {
			return "", nil
		}
		return *cached, nil
	}
	malware, err := osvQueryFn(pkg, ecosystem, version)
	if err != nil {
		slogDebug(fmt.Sprintf("OSV check failed for %s/%s (allowing): %v", ecosystem, pkg, err))
		return "", nil
	}
	var result *string
	if len(malware) > 0 {
		top := malware[:min(3, len(malware))]
		ids := make([]string, len(top))
		sums := make([]string, len(top))
		for i, m := range top {
			ids[i] = m["id"].(string)
			summary, ok := m["summary"]
			if !ok {
				summary = m["id"]
			}
			s, isStr := summary.(string)
			if !isStr {
				return "", fmt.Errorf("'%s' object is not subscriptable", pyTypeName(summary))
			}
			sums[i] = runeHead(s, 100)
		}
		msg := fmt.Sprintf("BLOCKED: Package '%s' (%s) has known malware advisories: %s. Details: %s", pkg, ecosystem, strings.Join(ids, ", "), strings.Join(sums, "; "))
		result = &msg
	}
	osvCachePut(key, result)
	if result == nil {
		return "", nil
	}
	return *result, nil
}

// Python type name of a decoded JSON value
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case float64:
		return "float"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return "str"
}

var ecosystemByCommand = map[string]string{
	"npx": "npm", "npx.cmd": "npm",
	"uvx": "PyPI", "uvx.cmd": "PyPI", "uvx.exe": "PyPI",
	"pipx": "PyPI", "pipx.exe": "PyPI",
}

var pathSepRe = regexp.MustCompile(`[\\/]`)

// Package ecosystem of a launcher command, empty for anything but npx, uvx and pipx
func inferEcosystem(command string) string {
	parts := pathSepRe.Split(command, -1)
	return ecosystemByCommand[strings.ToLower(parts[len(parts)-1])]
}

// Package name and version a launcher would run, empty when none is parseable
func parsePackageFromArgs(args []string, ecosystem string) (string, string) {
	token := ""
	takeNext := false
	for _, arg := range args {
		if takeNext {
			token = arg
			break
		}
		if arg == "--package" || arg == "-p" {
			takeNext = true
			continue
		}
		if strings.HasPrefix(arg, "--package=") {
			token = arg[len("--package="):]
			break
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		token = arg
		break
	}
	if token == "" {
		return "", ""
	}
	switch ecosystem {
	case "npm":
		return parseNpmPackage(token)
	case "PyPI":
		return parsePypiPackage(token)
	}
	return token, ""
}

var npmScopedRe = regexp.MustCompile(`^(@[^/]+/[^@]+)(?:@(.+))?$`)

var pypiPackageRe = regexp.MustCompile(`^([a-zA-Z0-9._-]+)(?:\[[^\]]*\])?(?:==(.+))?$`)

// Name and version of an npm token, latest meaning no pinned version
func parseNpmPackage(token string) (string, string) {
	if strings.HasPrefix(token, "@") {
		if m := npmScopedRe.FindStringSubmatch(token); m != nil {
			return m[1], m[2]
		}
		return token, ""
	}
	if i := strings.LastIndex(token, "@"); i >= 0 {
		name, version := token[:i], token[i+1:]
		if version == "latest" {
			version = ""
		}
		return name, version
	}
	return token, ""
}

// Name and version of a PyPI requirement token with optional extras
func parsePypiPackage(token string) (string, string) {
	if m := pypiPackageRe.FindStringSubmatch(token); m != nil {
		return m[1], m[2]
	}
	return token, ""
}

// MAL advisories OSV reports for a package, regular CVEs dropped
func queryOSV(pkg, ecosystem, version string) ([]map[string]any, error) {
	payload := map[string]any{"package": map[string]any{"name": pkg, "ecosystem": ecosystem}}
	if version != "" {
		payload["version"] = version
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, osvEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "icebath-agent-osv-check/1.0")
	resp, err := (&http.Client{Timeout: osvTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("'NoneType' object has no attribute 'get'")
	}
	vulns, ok := result["vulns"]
	if !ok {
		return nil, nil
	}
	list, ok := vulns.([]any)
	if !ok {
		return nil, fmt.Errorf("'%s' object is not iterable", pyTypeName(vulns))
	}
	var out []map[string]any
	for _, v := range list {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(v))
		}
		idRaw, ok := m["id"]
		if !ok {
			idRaw = ""
		}
		id, ok := idRaw.(string)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'startswith'", pyTypeName(idRaw))
		}
		if strings.HasPrefix(id, "MAL-") {
			out = append(out, m)
		}
	}
	return out, nil
}
