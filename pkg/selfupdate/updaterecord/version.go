package updaterecord

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Stable release tag shape, a three digit major cap keeping CalVer tags out
var StableTagRE = regexp.MustCompile(`^v(?:0|[1-9]\p{Nd}{0,2})\.(?:0|[1-9]\p{Nd}*)\.(?:0|[1-9]\p{Nd}*)$`)

// Release tag, canary build metadata or prerelease, git describe distance and dirty suffix of a build stamp
var buildTagRE = regexp.MustCompile(`^(v[0-9][0-9.]*)((?:\+canary\.[0-9TZ]+)|(?:-[A-Za-z][0-9A-Za-z.]*))?(?:-(\d+)-g([0-9a-f]{4,40}))?(-dirty)?$`)

var stampSources = []string{"commit-build", "ci", "docker", "fallback", "git", "local", "nix"}

var stampDistributions = []string{"docker", "nix", "desktop-app"}

type VersionInfo struct {
	BaseVersion    string
	DerivedVersion string
	Distance       *int64
	Commit         *string
	Branch         *string
	Source         string
	Dirty          bool
	CommitDate     *int64
	Distribution   *string
}

// RuntimeError a malformed install stamp raises
type StampError struct{ Msg string }

func (e *StampError) Error() string { return e.Msg }

// Release plus distance when a release is known, else the derived identity
func (v VersionInfo) DisplayVersion() string {
	if v.BaseVersion == "unknown" {
		return v.DerivedVersion
	}
	return derivedVersion(v.BaseVersion, v.Distance, false, "")
}

// Base version with the distance, short commit and dirty markers git describe would add
func derivedVersion(base string, distance *int64, dirty bool, shortCommit string) string {
	if distance != nil && *distance > 0 {
		suffix := strconv.FormatInt(*distance, 10)
		if shortCommit != "" {
			suffix = fmt.Sprintf("%d.g%s", *distance, shortCommit)
		}
		if dirty && shortCommit != "" {
			suffix += ".dirty"
		}
		return base + "+" + suffix
	}
	if dirty && distance == nil {
		return base + "+?"
	}
	return base
}

var (
	buildMu      sync.Mutex
	buildVersion string
	cacheMu      sync.Mutex
	cachedInfo   *VersionInfo
)

// Records the release tag or git describe text the build stamped into main.version
func SetBuildVersion(version string) {
	buildMu.Lock()
	buildVersion = version
	buildMu.Unlock()
	resetVersionInfoCache()
}

// Release tag or git describe text the build stamped, empty for an unstamped build
func BuildVersion() string {
	buildMu.Lock()
	defer buildMu.Unlock()
	return buildVersion
}

// Stamped version and the vcs settings the Go toolchain embedded, swapped by tests
var buildIdentity = func() (string, map[string]string) {
	buildMu.Lock()
	version := buildVersion
	buildMu.Unlock()
	settings := map[string]string{}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			settings[setting.Key] = setting.Value
		}
	}
	return version, settings
}

// Nonnegative Python int of a string, nil when it does not parse or is negative
func parseNonnegative(value string) *int64 {
	parsed, ok := pyint.Parse(value)
	if !ok || parsed.Sign() < 0 || !parsed.IsInt64() {
		return nil
	}
	n := parsed.Int64()
	return &n
}

// Install stamp of the executing tree when it is a regular file, swapped by tests
var resolveStampFile = func() string {
	path := pm.InstallStampPath(pm.RepoRoot())
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
		return path
	}
	return ""
}

func optionalString(value any) *string {
	if !pyrepr.Truthy(value) {
		return nil
	}
	s, ok := value.(string)
	if !ok {
		s = pyrepr.Str(value)
	}
	return &s
}

func jsonInt(value any) (int64, bool) {
	switch n := value.(type) {
	case int64:
		return n, true
	}
	return 0, false
}

// Provenance a build time install stamp records, nil without a usable stamp
func stampVersionInfo() (*VersionInfo, error) {
	path := resolveStampFile()
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	text, err := pyjson.DecodeBytes(append([]byte("\xef\xbb\xbf"), bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))...))
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, nil
	}
	data, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	if _, ok := data.Get("commit"); !ok {
		return nil, nil
	}
	mechanisms := pm.UpdateMechanisms()
	if mechanism, ok := data.Vals["updateMechanism"].(string); !ok || !slices.Contains(mechanisms, mechanism) {
		return nil, &StampError{Msg: fmt.Sprintf("install-stamp.json at %s is missing a valid 'updateMechanism' (one of %s). The build lane that wrote this stamp must pass --update-mechanism to scripts/write_install_stamp.py (or bake the field directly).",
			path, strings.Join(mechanisms, ", "))}
	}
	if payload, _ := data.Vals["payload"].(string); payload == "light" {
		return nil, &StampError{Msg: fmt.Sprintf("install-stamp.json at %s marks this artifact as 'light' (no agent runtime). No Python process can legitimately run from a light artifact — this build is mispackaged.", path)}
	}
	commitValue := data.Vals["commit"]
	if !pyrepr.Truthy(commitValue) {
		return nil, nil
	}
	commit, ok := commitValue.(string)
	if !ok {
		return nil, &pyjson.TypeError{Msg: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(commitValue))}
	}
	if strings.Trim(commit, "0") == "" {
		return nil, nil
	}
	stampSource := ""
	if value := data.Vals["source"]; pyrepr.Truthy(value) {
		stampSource = pyrepr.Str(value)
	}
	if stampSource == "git" {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), ".git")); err == nil {
			_, settings := buildIdentity()
			if live := settings["vcs.revision"]; live != "" && live != commit {
				return nil, nil
			}
		}
	}
	base := "unknown"
	if value := optionalString(data.Vals["baseVersion"]); value != nil {
		base = *value
	}
	display := base
	if value := optionalString(data.Vals["displayVersion"]); value != nil {
		display = *value
	}
	var distance *int64
	switch value := data.Vals["distance"].(type) {
	case string:
		distance = parseNonnegative(value)
	case int64:
		distance = &value
	}
	source := "build"
	if slices.Contains(stampSources, stampSource) {
		source = stampSource
	}
	var distribution *string
	if value, ok := data.Vals["distribution"].(string); ok && slices.Contains(stampDistributions, value) {
		distribution = &value
	}
	var commitDate *int64
	if value, ok := jsonInt(data.Vals["commitDate"]); ok {
		commitDate = &value
	}
	return &VersionInfo{BaseVersion: base, DerivedVersion: display, Distance: distance, Commit: &commit,
		Branch: optionalString(data.Vals["branch"]), Source: source, Dirty: pyrepr.Truthy(data.Vals["dirty"]),
		CommitDate: commitDate, Distribution: distribution}, nil
}

// Identity the binary carries from its stamped tag and embedded vcs revision, nil when it carries neither
func buildVersionInfo() *VersionInfo {
	version, settings := buildIdentity()
	revision := settings["vcs.revision"]
	if version == "" && revision == "" {
		return nil
	}
	dirty := settings["vcs.modified"] == "true"
	short := ""
	if len(revision) >= 7 {
		short = revision[:7]
	}
	base, source := "unknown", "git"
	var distance *int64
	if match := buildTagRE.FindStringSubmatch(version); match != nil && StableTagRE.MatchString(match[1]) {
		base = match[1][1:]
		switch {
		case match[3] != "":
			distance = parseNonnegative(match[3])
		case match[2] == "":
			zero := int64(0)
			distance, source = &zero, "build"
		}
		if short == "" {
			short = match[4]
		}
		dirty = dirty || match[5] != ""
	}
	var commit *string
	if revision != "" {
		commit = &revision
	}
	var commitDate *int64
	if stamp, err := time.Parse(time.RFC3339, settings["vcs.time"]); err == nil {
		seconds := stamp.Unix()
		commitDate = &seconds
	}
	display := derivedVersion(base, distance, dirty, short)
	if base == "unknown" && short != "" {
		display = "git." + short
		if dirty {
			display += ".dirty"
		}
	}
	return &VersionInfo{BaseVersion: base, DerivedVersion: display, Distance: distance, Commit: commit,
		Source: source, Dirty: dirty, CommitDate: commitDate}
}

func resetVersionInfoCache() {
	cacheMu.Lock()
	cachedInfo = nil
	cacheMu.Unlock()
}

// Stamp provenance, nil when the stamp is missing or malformed in a way the build lane owns
func stampOrNone() (*VersionInfo, error) {
	info, err := stampVersionInfo()
	var stampErr *StampError
	if errors.As(err, &stampErr) {
		return nil, nil
	}
	return info, err
}

// Cached provenance from the install stamp, then the build identity, then unknown
func GetVersionInfo() (VersionInfo, error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cachedInfo != nil {
		return *cachedInfo, nil
	}
	info, err := stampOrNone()
	if err != nil {
		return VersionInfo{}, err
	}
	if info == nil {
		info = buildVersionInfo()
	}
	if info == nil {
		info = &VersionInfo{BaseVersion: "unknown", DerivedVersion: "unknown", Source: "unknown"}
	}
	cachedInfo = info
	return *info, nil
}

// Product slash base version for an outbound request header
func UserAgent(product string) (string, error) {
	info, err := GetVersionInfo()
	if err != nil {
		return "", err
	}
	return product + "/" + info.BaseVersion, nil
}

// Flat sha, short sha, version, source and commit date identity, re-resolved when refresh is set
func GetCodeIdentity(refresh bool) (*pyjson.Object, error) {
	if refresh {
		resetVersionInfoCache()
	}
	info, err := GetVersionInfo()
	if err != nil {
		return nil, err
	}
	identity := pyjson.NewObject()
	var sha, short, commitDate any
	if info.Commit != nil {
		sha = *info.Commit
		short = (*info.Commit)[:min(8, len(*info.Commit))]
	}
	if info.CommitDate != nil {
		commitDate = *info.CommitDate
	}
	identity.Set("sha", sha)
	identity.Set("short_sha", short)
	identity.Set("version", info.BaseVersion)
	identity.Set("source", info.Source)
	identity.Set("commit_date", commitDate)
	return identity, nil
}

// Commit of the running install re-read now, the stamp's first and the build revision second
func ReadCheckoutSHA() (*string, error) {
	resetVersionInfoCache()
	stamp, err := stampOrNone()
	if err != nil {
		return nil, err
	}
	if stamp != nil {
		return stamp.Commit, nil
	}
	if _, settings := buildIdentity(); settings["vcs.revision"] != "" {
		revision := settings["vcs.revision"]
		return &revision, nil
	}
	return nil, nil
}
