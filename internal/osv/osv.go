// Package osv is the malware check of packages: whether the OSV database lists a package a launcher runs or an installer fetches as
// malware, each answer kept for a while in the profile's cache
package osv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	osvconfig "github.com/nickheyer/icebath/internal/osv/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Ecosystems of the launchers the check knows, by command name
var launchers = map[string]string{"npx": "npm", "npx.cmd": "npm", "uvx": "PyPI", "uvx.cmd": "PyPI", "uvx.exe": "PyPI", "pipx": "PyPI", "pipx.exe": "PyPI"}

var (
	npmScoped   = regexp.MustCompile(`^(@[^/]+/[^@]+)(?:@(.+))?$`)
	pypiPackage = regexp.MustCompile(`^([a-zA-Z0-9._-]+)(?:\[[^\]]*\])?(?:==(.+))?$`)
	pypiName    = regexp.MustCompile(`^[a-zA-Z0-9._-]+`)
	// An npm version OSV can look up, rather than a range or a tag such as latest
	exact = regexp.MustCompile(`^v?\d+(?:\.\d+)*(?:[-+][0-9A-Za-z.-]+)?$`)
)

// Most answers the cache keeps
const kept = 256

// Ecosystem of a launcher command such as npx, uvx or pipx, empty for any other
func Ecosystem(command string) string {
	return launchers[strings.ToLower(filepath.Base(strings.ReplaceAll(command, `\`, "/")))]
}

// Options of each launcher whose value is the package it runs, and those whose value is anything else
var (
	packageOptions = map[string][]string{"npx": {"--package", "-p"}, "uvx": {"--from"}, "pipx": {"--spec"}}
	valueOptions   = map[string][]string{"npx": {"--call", "-c", "--cache", "--registry"},
		"uvx": {"--python", "-p", "--with", "--index-url", "--extra-index-url", "-i"}, "pipx": {"--python", "--pip-args", "--index-url", "-i"}}
)

// Package a launch of npx, uvx or pipx runs: the value of the launcher's package option, else the first argument that is no option or
// its value, pipx's subcommand left out
func Launched(command string, args []string) string {
	launcher := strings.ToLower(filepath.Base(strings.ReplaceAll(command, `\`, "/")))
	launcher = strings.TrimSuffix(strings.TrimSuffix(launcher, ".cmd"), ".exe")
	subcommand := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if slices.Contains(packageOptions[launcher], arg) {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		for _, option := range packageOptions[launcher] {
			if value, ok := strings.CutPrefix(arg, option+"="); ok && strings.HasPrefix(option, "--") {
				return value
			}
		}
		if strings.HasPrefix(arg, "-") {
			if slices.Contains(valueOptions[launcher], arg) {
				i++
			}
			continue
		}
		if launcher == "pipx" && !subcommand && (arg == "run" || arg == "install" || arg == "inject") {
			subcommand = true
			continue
		}
		return arg
	}
	return ""
}

// Message refusing the package an npx, uvx or pipx launch runs when OSV lists malware advisories for it, empty when the package is clean,
// the command is no launcher, or the check could not be made
func Launch(ctx context.Context, cfg *config.Config, command string, args []string) string {
	ecosystem := Ecosystem(command)
	if ecosystem == "" {
		return ""
	}
	if token := Launched(command, args); token != "" {
		return Check(ctx, cfg, ecosystem, token)
	}
	return ""
}

// Name and version of a package as a launcher or an installer of the ecosystem writes it: name@version for npm, name[extras]==version for
// PyPI; the version empty unless it names one release
func Parse(ecosystem, token string) (string, string) {
	name, version := token, ""
	switch {
	case ecosystem == "npm" && strings.HasPrefix(token, "@"):
		if found := npmScoped.FindStringSubmatch(token); found != nil {
			name, version = found[1], found[2]
		}
	case ecosystem == "npm":
		if at := strings.LastIndex(token, "@"); at > 0 {
			name, version = token[:at], token[at+1:]
		}
	default:
		if found := pypiPackage.FindStringSubmatch(token); found != nil {
			name, version = found[1], found[2]
		} else {
			name = pypiName.FindString(token)
		}
		if strings.ContainsAny(version, "*,<>!~= ") {
			version = ""
		}
		return name, version
	}
	if !exact.MatchString(version) {
		version = ""
	}
	return name, version
}

// One answer of the check kept on disk: when it expires, and the refusal, nil for a clean package
type answer struct {
	Expiry float64 `json:"expiry"`
	Result *string `json:"result"`
}

type cache struct {
	Version int               `json:"version"`
	Entries map[string]answer `json:"entries"`
}

func cachePath(cfg *config.Config) string {
	return filepath.Join(cfg.ProfileDir(cfg.Profile()), "cache", "osv_check.json")
}

// Answers kept on disk, none when the file is missing or unreadable
func load(path string) map[string]answer {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]answer{}
	}
	var disk cache
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF")), &disk) != nil || disk.Version != 1 || disk.Entries == nil {
		return map[string]answer{}
	}
	return disk.Entries
}

// Keeps one answer, read again from disk first so another process's answers stay, the expired ones and the oldest beyond the cap dropped
func store(path, key string, value answer, now float64) {
	entries := load(path)
	entries[key] = value
	maps.DeleteFunc(entries, func(_ string, a answer) bool { return a.Expiry <= now })
	if len(entries) > kept {
		keys := slices.SortedFunc(maps.Keys(entries), func(a, b string) int {
			switch ea, eb := entries[a].Expiry, entries[b].Expiry; {
			case ea < eb:
				return -1
			case ea > eb:
				return 1
			}
			return strings.Compare(a, b)
		})
		for _, old := range keys[:len(entries)-kept] {
			delete(entries, old)
		}
	}
	data, err := json.Marshal(cache{Version: 1, Entries: entries})
	if err == nil {
		err = fsx.Write(path, data, 0o644)
	}
	if err != nil {
		slog.Debug("Failed to save the OSV check cache", "path", path, "error", err)
	}
}

// Message refusing a package of the ecosystem, npm or PyPI, written as a launcher or installer writes it, when OSV lists malware
// advisories for it; empty when it is clean or could not be checked, the check never standing in the way of a package it cannot reach
func Check(ctx context.Context, cfg *config.Config, ecosystem, token string) string {
	name, version := Parse(ecosystem, token)
	if name == "" {
		return ""
	}
	key := ecosystem + "|" + name + "|" + version
	path := cachePath(cfg)
	now := float64(time.Now().UnixNano()) / 1e9
	if cached, ok := load(path)[key]; ok && now < cached.Expiry {
		if cached.Result == nil {
			return ""
		}
		return *cached.Result
	}
	advisories, err := query(ctx, cfg.GetString(osvconfig.Endpoint), name, ecosystem, version)
	if err != nil {
		slog.Debug("OSV check failed, allowing the package", "ecosystem", ecosystem, "package", name, "error", err)
		return ""
	}
	var result *string
	if len(advisories) > 0 {
		top := advisories[:min(3, len(advisories))]
		ids, summaries := make([]string, len(top)), make([]string, len(top))
		for i, a := range top {
			ids[i] = a.ID
			summary := a.Summary
			if summary == "" {
				summary = a.ID
			}
			runes := []rune(summary)
			summaries[i] = strings.TrimRight(strings.TrimSpace(string(runes[:min(100, len(runes))])), ".;")
		}
		message := fmt.Sprintf("The %s package %s has known malware advisories, %s, so it was blocked. What they say: %s.", ecosystem, name,
			strings.Join(ids, ", "), strings.Join(summaries, ". "))
		result = &message
	}
	store(path, key, answer{Expiry: now + float64(cfg.GetInt(osvconfig.CacheTTL)), Result: result}, now)
	if result == nil {
		return ""
	}
	return *result
}

type advisory struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

// MAL advisories OSV knows of a package, the other advisories left out
func query(ctx context.Context, endpoint, name, ecosystem, version string) ([]advisory, error) {
	payload := map[string]any{"package": map[string]any{"name": name, "ecosystem": ecosystem}}
	if version != "" {
		payload["version"] = version
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "icebath-agent-osv-check/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("OSV answered %d %s.", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var result struct {
		Vulns []advisory `json:"vulns"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(result.Vulns, func(v advisory) bool { return !strings.HasPrefix(v.ID, "MAL-") }), nil
}
