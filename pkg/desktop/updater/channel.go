package updater

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	channelReadLimit       = 4 * 1024 * 1024
	channelReadTimeout     = 30 * time.Second
	channelCommandTimeout  = 120 * time.Second
	channelPowerShellLimit = 600 * time.Second
	pinnedArtifactTimeout  = 600 * time.Second
	maxSafeInteger         = 1<<53 - 1
)

var (
	sha256RE          = regexp.MustCompile(`^[a-f0-9]{64}$`)
	commitRE          = regexp.MustCompile(`^[a-f0-9]{40}$`)
	buildIDRE         = regexp.MustCompile(`^[a-f0-9]{32}$`)
	semverRE          = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(?:\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`)
	archiveRefRE      = regexp.MustCompile(`^rc\.(?:[1-9]\d*)-v(?:0|[1-9]\d{0,2})\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)$`)
	repositoryRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	anyTextRE         = regexp.MustCompile(`^[\s\S]+$`)
	releaseTagRE      = regexp.MustCompile(`^v\d+\.\d+\.\d+(?:\+canary\.20\d{6}T\d{6}Z)?$`)
	windowsStableRE   = regexp.MustCompile(`^\d+\.\d+\.\d+\.0$`)
	channelKeyRE      = regexp.MustCompile(`^releases/[A-Za-z0-9_./+-]+$`)
	reservedPartRE    = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\.|$)`)
	unsafeBaseRE      = regexp.MustCompile(`[%\\;\s]`)
	teamIDRE          = regexp.MustCompile(`^[A-Z0-9]{10}$`)
	feedChannelRE     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	threePartRE       = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	retiredBundleKeys = map[string]bool{"ICEBATH_SKIP_INTRO": true, "ICEBATH_GUEST_ONBOARDING": true}
	allowedBundleKeys = map[string]bool{"ICEBATH_HOME": true, "ICEBATH_DATA_DIR_SUFFIX": true, "ICEBATH_DESKTOP_USER_DATA_DIR": true, "ICEBATH_SHARED_AUTH_DIR": true, "ICEBATH_PREVIEW_FULL_CONNECTORS": true}
)

// ChannelIdentity is the product identity a channel build is bound to
type ChannelIdentity struct {
	Token                 string `json:"token"`
	DisplayName           string `json:"displayName"`
	AppID                 string `json:"appId"`
	AppNamePascal         string `json:"appNamePascal"`
	ArtifactNamePascal    string `json:"artifactNamePascal"`
	CliName               string `json:"cliName"`
	WindowsExecutableName string `json:"windowsExecutableName"`
	MsixAppIDWithOrg      string `json:"msixAppIdWithOrg"`
}

// ChannelBuild is the channel build request baked into a channel artifact's stamp
type ChannelBuild struct {
	BuildID        string             `json:"buildId"`
	Channel        string             `json:"channel"`
	Sequence       int64              `json:"sequence"`
	Repository     string             `json:"repository"`
	Commit         string             `json:"commit"`
	SourceVersion  string             `json:"sourceVersion"`
	Version        string             `json:"version"`
	WindowsVersion string             `json:"windowsVersion"`
	Identity       ChannelIdentity    `json:"identity"`
	BundleEnv      map[string]*string `json:"bundleEnv"`
	PublicBase     string             `json:"publicBase"`
}

// ChannelRequest is the build request a channel manifest carries
type ChannelRequest struct {
	ChannelBuild
	ControllerCommit string
	ReleaseTag       string
	ArchiveRef       string
}

// ChannelHead is the published head of a channel
type ChannelHead struct {
	BuildID     string
	Sequence    int64
	ManifestKey string
	SHA256      string
}

// ChannelRecord is an active or retired channel record
type ChannelRecord struct {
	Name             string
	Repository       string
	Policy           string
	Revision         int64
	NextSequence     int64
	Identity         ChannelIdentity
	Head             *ChannelHead
	State            string
	Destination      string
	MinimumVersion   string
	DestinationHead  *ChannelHead
	ReceiverProtocol int64
	ReceiverKind     string
	LastHead         *ChannelHead
}

// ChannelArtifact is the pinned native artifact of a channel package
type ChannelArtifact struct {
	Key    string
	SHA256 string
	Size   int64
}

// ChannelPackage is one platform package of a channel manifest
type ChannelPackage struct {
	Platform    string
	Arch        string
	Version     string
	Identity    string
	Artifact    ChannelArtifact
	Publisher   string
	TeamID      string
	FeedKey     string
	FeedChannel string
}

// ChannelManifest is the build manifest a channel head names
type ChannelManifest struct {
	Request          ChannelRequest
	Packages         []ChannelPackage
	ReceiverProtocol int64
}

// ChannelTarget is the resolved package this install would move to
type ChannelTarget struct {
	Channel        ChannelRecord
	Manifest       ChannelManifest
	Package        ChannelPackage
	ManifestSHA256 string
	ArtifactURL    string
	FeedURL        string
}

// ChannelRetirementStatus is the retirement the status reports for a discontinued channel
type ChannelRetirementStatus struct {
	State       string `json:"state"`
	Destination string `json:"destination"`
	Version     string `json:"version"`
	Message     string `json:"message,omitempty"`
}

// Parses channel JSON refusing a duplicate member in any object
func parseChannelJSON(body string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	value, err := decodeUnique(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("Unexpected non-whitespace character after JSON")
	}
	return value, nil
}

func decodeUnique(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch delim := tok.(type) {
	case json.Delim:
		if delim == '{' {
			out := map[string]any{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key := keyTok.(string)
				if _, seen := out[key]; seen {
					return nil, errors.New("Duplicate channel JSON member")
				}
				if out[key], err = decodeUnique(dec); err != nil {
					return nil, err
				}
			}
			_, err := dec.Token()
			return out, err
		}
		var out []any
		for dec.More() {
			item, err := decodeUnique(dec)
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
		_, err := dec.Token()
		return out, err
	}
	return tok, nil
}

// Typed reads of one untrusted channel object
type channelFields map[string]any

func newChannelFields(value any) (channelFields, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("Expected channel object")
	}
	return channelFields(object), nil
}

func (f channelFields) text(key string, pattern *regexp.Regexp) (string, error) {
	value, ok := f[key].(string)
	if !ok || !utf8.ValidString(value) {
		return "", fmt.Errorf("Invalid channel %s", key)
	}
	for _, r := range value {
		if r < 32 {
			return "", fmt.Errorf("Invalid channel %s", key)
		}
	}
	if pattern == nil {
		if units := len(utf16.Encode([]rune(value))); units < 1 || units > 2048 {
			return "", fmt.Errorf("Invalid channel %s", key)
		}
	} else if !pattern.MatchString(value) {
		return "", fmt.Errorf("Invalid channel %s", key)
	}
	return value, nil
}

func (f channelFields) integer(key string, maximum int64) (int64, error) {
	number, ok := f[key].(json.Number)
	if !ok {
		return 0, fmt.Errorf("Invalid channel %s", key)
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || value != math.Trunc(value) || value < 1 || value > float64(maximum) || value > maxSafeInteger {
		return 0, fmt.Errorf("Invalid channel %s", key)
	}
	return int64(value), nil
}

func (f channelFields) object(key string) (channelFields, error) { return newChannelFields(f[key]) }

func (f channelFields) optional(key string, pattern *regexp.Regexp) (string, error) {
	if _, present := f[key]; !present {
		return "", nil
	}
	return f.text(key, pattern)
}

func (f channelFields) schema() error {
	if number, ok := f["schema"].(json.Number); !ok || number.String() != "1" {
		return errors.New("Unsupported channel schema")
	}
	return nil
}

func validateChannelName(name string) (string, error) {
	return name, validateDarwinFeedChannel(name)
}

// Canonical channel archive base, HTTPS or loopback HTTP without credentials, query or traversal
func channelPublicBase(value string) (string, error) {
	parsed, err := url.Parse(value)
	invalid := errors.New("Channel archive must use HTTPS or loopback HTTP without credentials or traversal")
	if err != nil || parsed.Host == "" {
		return "", invalid
	}
	loopback := slices.Contains([]string{"127.0.0.1", "localhost", "[::1]"}, hostnameWithBrackets(parsed))
	if (parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback)) || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(value, "#") || unsafeBaseRE.MatchString(value) {
		return "", invalid
	}
	for _, part := range strings.Split(value, "/") {
		if part == "." || part == ".." {
			return "", invalid
		}
	}
	return strings.TrimRight(value, "/"), nil
}

func hostnameWithBrackets(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// Object key of the channel archive, a releases path with no traversal or reserved names
func channelKey(value string) (string, error) {
	if !channelKeyRE.MatchString(value) {
		return "", errors.New("Invalid channel object key")
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || strings.HasSuffix(part, ".") || reservedPartRE.MatchString(part) {
			return "", errors.New("Invalid channel object key")
		}
	}
	return value, nil
}

func buildPrefix(buildID string) (string, error) {
	if !buildIDRE.MatchString(buildID) {
		return "", errors.New("Invalid channel buildId")
	}
	return "releases/channel-builds/" + buildID + "/", nil
}

func decodeIdentity(f channelFields) (ChannelIdentity, error) {
	var id ChannelIdentity
	var err error
	read := func(key, pattern string) string {
		if err != nil {
			return ""
		}
		var value string
		value, err = f.text(key, regexp.MustCompile(pattern))
		return value
	}
	id.Token = read("token", `^[a-f0-9]{16}$`)
	id.DisplayName = read("displayName", `^[A-Za-z0-9][A-Za-z0-9 ._-]{0,79}$`)
	id.AppID = read("appId", `^[a-z][a-z0-9.-]{2,127}$`)
	id.AppNamePascal = read("appNamePascal", `^[A-Za-z][A-Za-z0-9]{0,63}$`)
	id.ArtifactNamePascal = read("artifactNamePascal", `^[A-Za-z][A-Za-z0-9]{0,63}$`)
	id.CliName = read("cliName", `^[a-z][a-z0-9-]{0,63}$`)
	id.WindowsExecutableName = read("windowsExecutableName", `^[A-Za-z][A-Za-z0-9 ._-]{0,79}$`)
	id.MsixAppIDWithOrg = read("msixAppIdWithOrg", `^[A-Za-z][A-Za-z0-9.-]{2,49}$`)
	return id, err
}

func sameChannelIdentity(left, right ChannelIdentity) bool { return left == right }

func decodeHead(value any) (*ChannelHead, error) {
	if value == nil {
		return nil, nil
	}
	f, err := newChannelFields(value)
	if err != nil {
		return nil, err
	}
	buildID, err := f.text("buildId", buildIDRE)
	if err != nil {
		return nil, err
	}
	manifestKey, err := f.text("manifestKey", nil)
	if err != nil {
		return nil, err
	}
	prefix, err := buildPrefix(buildID)
	if err != nil {
		return nil, err
	}
	if manifestKey != prefix+"build.json" {
		return nil, errors.New("Invalid channel manifestKey")
	}
	sequence, err := f.integer("sequence", 0xffffffff)
	if err != nil {
		return nil, err
	}
	sum, err := f.text("sha256", sha256RE)
	if err != nil {
		return nil, err
	}
	return &ChannelHead{BuildID: buildID, ManifestKey: manifestKey, Sequence: sequence, SHA256: sum}, nil
}

// Decodes and validates a channel record, active or retired
func decodeChannelRecord(body string) (ChannelRecord, error) {
	var r ChannelRecord
	parsed, err := parseChannelJSON(body)
	if err != nil {
		return r, err
	}
	f, err := newChannelFields(parsed)
	if err != nil {
		return r, err
	}
	if err := f.schema(); err != nil {
		return r, err
	}
	if r.Policy, err = f.text("policy", nil); err != nil {
		return r, err
	}
	if r.Policy != "preview" && r.Policy != "stable-release" && r.Policy != "canary-release" {
		return r, errors.New("Channel has no supported desktop delivery")
	}
	if r.Name, err = f.text("name", nil); err != nil {
		return r, err
	}
	if _, err = validateChannelName(r.Name); err != nil {
		return r, err
	}
	if r.Repository, err = f.text("repository", repositoryRE); err != nil {
		return r, err
	}
	identity, err := f.object("identity")
	if err != nil {
		return r, err
	}
	if r.Identity, err = decodeIdentity(identity); err != nil {
		return r, err
	}
	if r.Revision, err = f.integer("revision", maxSafeInteger); err != nil {
		return r, err
	}
	if r.NextSequence, err = f.integer("nextSequence", 0x100000000); err != nil {
		return r, err
	}
	if r.Head, err = decodeHead(f["head"]); err != nil {
		return r, err
	}
	if r.Head != nil && r.Head.Sequence >= r.NextSequence {
		return r, errors.New("Channel head exceeds allocation")
	}
	if r.State, err = f.text("state", nil); err != nil {
		return r, err
	}
	if r.State == "active" {
		return r, nil
	}
	if r.State != "retired" {
		return r, errors.New("Invalid channel state")
	}
	if r.LastHead, err = decodeHead(f["lastHead"]); err != nil {
		return r, err
	}
	if (r.LastHead == nil) != (r.Head == nil) || (r.LastHead != nil && *r.LastHead != *r.Head) {
		return r, errors.New("Retirement lastHead mismatch")
	}
	if number, ok := f["receiverProtocol"].(json.Number); !ok || number.String() != "1" {
		return r, errors.New("Unsupported retirement receiver protocol")
	}
	r.ReceiverProtocol = 1
	receiver, err := f.object("receiver")
	if err != nil {
		return r, err
	}
	if r.ReceiverKind, err = receiver.text("kind", nil); err != nil {
		return r, err
	}
	if r.ReceiverKind != "in-place" && r.ReceiverKind != "discontinued" {
		return r, errors.New("Invalid retirement receiver kind")
	}
	if r.Destination, err = f.text("destination", nil); err != nil {
		return r, err
	}
	if _, err = validateChannelName(r.Destination); err != nil {
		return r, err
	}
	if r.MinimumVersion, err = f.text("minimumVersion", semverRE); err != nil {
		return r, err
	}
	if r.DestinationHead, err = decodeHead(f["destinationHead"]); err != nil {
		return r, err
	}
	if r.DestinationHead == nil {
		return r, errors.New("Missing retirement destination head")
	}
	return r, nil
}

func decodeRequest(f channelFields) (ChannelRequest, error) {
	var q ChannelRequest
	if err := f.schema(); err != nil {
		return q, err
	}
	environment, err := f.object("bundleEnv")
	if err != nil {
		return q, err
	}
	q.BundleEnv = map[string]*string{}
	for key, value := range environment {
		if retiredBundleKeys[key] {
			continue
		}
		if !allowedBundleKeys[key] {
			return q, errors.New("Invalid bundle environment name")
		}
		if value == nil {
			q.BundleEnv[key] = nil
			continue
		}
		text, ok := value.(string)
		if !ok || strings.Contains(text, "\x00") {
			return q, errors.New("Invalid bundle environment value")
		}
		q.BundleEnv[key] = &text
	}
	if q.PublicBase, err = f.text("publicBase", nil); err != nil {
		return q, err
	}
	if canonical, err := channelPublicBase(q.PublicBase); err != nil {
		return q, err
	} else if canonical != q.PublicBase {
		return q, errors.New("Noncanonical request publicBase")
	}
	if q.ReleaseTag, err = f.optional("releaseTag", releaseTagRE); err != nil {
		return q, err
	}
	windowsPattern := windowsStableRE
	if strings.Contains(q.ReleaseTag, "+canary.") {
		windowsPattern = windowsVersionRE
	}
	if q.WindowsVersion, err = f.text("windowsVersion", windowsPattern); err != nil {
		return q, err
	}
	for _, part := range strings.Split(q.WindowsVersion, ".") {
		if n, _ := strconv.Atoi(part); n > 65535 {
			return q, errors.New("Invalid Windows version")
		}
	}
	steps := []struct {
		key     string
		pattern *regexp.Regexp
		into    *string
	}{
		{"buildId", buildIDRE, &q.BuildID}, {"channel", nil, &q.Channel}, {"repository", repositoryRE, &q.Repository},
		{"commit", commitRE, &q.Commit}, {"sourceVersion", semverRE, &q.SourceVersion}, {"version", semverRE, &q.Version},
	}
	for _, step := range steps {
		if *step.into, err = f.text(step.key, step.pattern); err != nil {
			return q, err
		}
	}
	if _, err := validateChannelName(q.Channel); err != nil {
		return q, err
	}
	if q.Sequence, err = f.integer("sequence", 0xffffffff); err != nil {
		return q, err
	}
	identity, err := f.object("identity")
	if err != nil {
		return q, err
	}
	if q.Identity, err = decodeIdentity(identity); err != nil {
		return q, err
	}
	if q.ControllerCommit, err = f.optional("controllerCommit", commitRE); err != nil {
		return q, err
	}
	if q.ArchiveRef, err = f.optional("archiveRef", archiveRefRE); err != nil {
		return q, err
	}
	return q, nil
}

func decodePackage(value any) (ChannelPackage, error) {
	var p ChannelPackage
	f, err := newChannelFields(value)
	if err != nil {
		return p, err
	}
	if p.Platform, err = f.text("platform", nil); err != nil {
		return p, err
	}
	if p.Arch, err = f.text("arch", nil); err != nil {
		return p, err
	}
	if (p.Platform != "darwin" && p.Platform != "win32") || (p.Arch != "x64" && p.Arch != "arm64") || f["variant"] != "bundled" {
		return p, errors.New("Unsupported channel package")
	}
	artifact, err := f.object("artifact")
	if err != nil {
		return p, err
	}
	feed, err := f.object("feed")
	if err != nil {
		return p, err
	}
	versionPattern := semverRE
	if p.Platform != "darwin" {
		versionPattern = windowsVersionRE
	}
	if p.Version, err = f.text("version", versionPattern); err != nil {
		return p, err
	}
	if p.Identity, err = f.text("identity", nil); err != nil {
		return p, err
	}
	key, err := artifact.text("key", nil)
	if err != nil {
		return p, err
	}
	if p.Artifact.Key, err = channelKey(key); err != nil {
		return p, err
	}
	if p.Artifact.SHA256, err = artifact.text("sha256", sha256RE); err != nil {
		return p, err
	}
	if p.Artifact.Size, err = artifact.integer("size", maxSafeInteger); err != nil {
		return p, err
	}
	if p.Publisher, err = f.optional("publisher", nil); err != nil {
		return p, err
	}
	if p.TeamID, err = f.optional("teamId", teamIDRE); err != nil {
		return p, err
	}
	feedKey, err := feed.text("key", nil)
	if err != nil {
		return p, err
	}
	if p.FeedKey, err = channelKey(feedKey); err != nil {
		return p, err
	}
	p.FeedChannel, err = feed.text("channel", feedChannelRE)
	return p, err
}

// Decodes and validates a channel build manifest
func decodeChannelManifest(body string) (ChannelManifest, error) {
	var m ChannelManifest
	parsed, err := parseChannelJSON(body)
	if err != nil {
		return m, err
	}
	f, err := newChannelFields(parsed)
	if err != nil {
		return m, err
	}
	if err := f.schema(); err != nil {
		return m, err
	}
	entries, ok := f["packages"].([]any)
	if !ok || len(entries) == 0 || len(entries) > 4 {
		return m, errors.New("Invalid channel packages")
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		pkg, err := decodePackage(entry)
		if err != nil {
			return m, err
		}
		key := pkg.Platform + "/" + pkg.Arch
		if seen[key] {
			return m, errors.New("Duplicate channel package")
		}
		seen[key] = true
		m.Packages = append(m.Packages, pkg)
	}
	request, err := f.object("request")
	if err != nil {
		return m, err
	}
	if m.Request, err = decodeRequest(request); err != nil {
		return m, err
	}
	if _, present := f["receiverProtocol"]; present {
		if m.ReceiverProtocol, err = f.integer("receiverProtocol", maxSafeInteger); err != nil {
			return m, err
		}
	}
	return m, nil
}

// Reads channel records and manifests from the build's public archive
type channelResolver struct {
	build    *ChannelBuild
	base     string
	platform string
	arch     string
	signer   string
}

func newChannelResolver(build *ChannelBuild, platform, arch, signer string) (*channelResolver, error) {
	base, err := channelPublicBase(build.PublicBase)
	if err != nil {
		return nil, err
	}
	if _, err := validateChannelName(build.Channel); err != nil {
		return nil, err
	}
	return &channelResolver{build: build, base: base, platform: platform, arch: arch, signer: signer}, nil
}

// One uncached archive object of at most 4 MiB, checked against digest when one is given
func (r *channelResolver) read(key, digest string) (string, error) {
	checked, err := channelKey(key)
	if err != nil {
		return "", err
	}
	target := r.base + "/" + checked
	ctx, cancel := context.WithTimeout(context.Background(), channelReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := noRedirectClient(0).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 || resp.Request.URL.String() != target {
		return "", fmt.Errorf("Channel read unavailable: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, channelReadLimit+1))
	if err != nil {
		return "", err
	}
	if len(body) > channelReadLimit {
		return "", errors.New("Channel metadata exceeds size limit")
	}
	if digest != "" {
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != digest {
			return "", errors.New("Channel metadata SHA256 mismatch")
		}
	}
	if !utf8.Valid(body) {
		return "", errors.New("The encoded data was not valid for encoding utf-8")
	}
	return strings.TrimPrefix(string(body), "\uFEFF"), nil
}

// Outcome of a channel resolution, an active target, an empty channel or a retirement
type channelResolution struct {
	kind         string
	target       *ChannelTarget
	receiverKind string
}

func assertVersionFloor(version, floor string) error {
	if !threePartRE.MatchString(version) || !threePartRE.MatchString(floor) {
		return errors.New("Retirement version floor is not comparable")
	}
	current, minimum := strings.Split(version, "."), strings.Split(floor, ".")
	for i := 0; i < 3; i++ {
		a, _ := new(big.Int).SetString(current[i], 10)
		b, _ := new(big.Int).SetString(minimum[i], 10)
		switch a.Cmp(b) {
		case 1:
			return nil
		case -1:
			return errors.New("Destination is below retirement minimum version")
		}
	}
	return nil
}

func (r *channelResolver) assertRecord(record ChannelRecord, name string) error {
	if record.Name != name {
		return errors.New("Channel name does not match object key")
	}
	if !strings.EqualFold(record.Repository, r.build.Repository) {
		return errors.New("Channel repository authority mismatch")
	}
	return nil
}

func (r *channelResolver) resolve() (channelResolution, error) {
	body, err := r.read("releases/channels/"+r.build.Channel+".json", "")
	if err != nil {
		return channelResolution{}, err
	}
	record, err := decodeChannelRecord(body)
	if err != nil {
		return channelResolution{}, err
	}
	if err := r.assertRecord(record, r.build.Channel); err != nil {
		return channelResolution{}, err
	}
	if !sameChannelIdentity(record.Identity, r.build.Identity) {
		return channelResolution{}, errors.New("Installed channel identity changed")
	}
	if record.State == "active" {
		if record.Head == nil {
			return channelResolution{kind: "empty"}, nil
		}
		target, err := r.target(record)
		return channelResolution{kind: "active", target: target}, err
	}
	retired := record
	if retired.Destination == retired.Name {
		return channelResolution{}, errors.New("Channel retirement cycle")
	}
	if body, err = r.read("releases/channels/"+retired.Destination+".json", ""); err != nil {
		return channelResolution{}, err
	}
	if record, err = decodeChannelRecord(body); err != nil {
		return channelResolution{}, err
	}
	if err := r.assertRecord(record, retired.Destination); err != nil {
		return channelResolution{}, err
	}
	if record.State != "active" || record.Policy != "stable-release" {
		return channelResolution{}, errors.New("Retirement requires a directly active stable-release destination")
	}
	if record.Head == nil || retired.DestinationHead.Sequence > record.Head.Sequence {
		return channelResolution{}, errors.New("Retirement destination exceeds published head")
	}
	pinned := record
	pinned.Head = retired.DestinationHead
	target, err := r.target(pinned)
	if err != nil {
		return channelResolution{}, err
	}
	if target.Manifest.ReceiverProtocol != retired.ReceiverProtocol {
		return channelResolution{}, errors.New("Stable build has no supported retirement receiver")
	}
	if err := assertVersionFloor(target.Manifest.Request.SourceVersion, retired.MinimumVersion); err != nil {
		return channelResolution{}, err
	}
	return channelResolution{kind: "retirement", target: target, receiverKind: retired.ReceiverKind}, nil
}

func (r *channelResolver) target(channel ChannelRecord) (*ChannelTarget, error) {
	if channel.Head == nil {
		return nil, errors.New("No destination build published")
	}
	body, err := r.read(channel.Head.ManifestKey, channel.Head.SHA256)
	if err != nil {
		return nil, err
	}
	manifest, err := decodeChannelManifest(body)
	if err != nil {
		return nil, err
	}
	q := manifest.Request
	if q.BuildID != channel.Head.BuildID || q.Sequence != channel.Head.Sequence || q.Channel != channel.Name ||
		!strings.EqualFold(q.Repository, channel.Repository) || q.PublicBase != r.base || !sameChannelIdentity(q.Identity, channel.Identity) {
		return nil, errors.New("Channel manifest request binding mismatch")
	}
	if channel.Policy == "preview" && (q.Version != fmt.Sprintf("0.0.%d", q.Sequence) || q.WindowsVersion != fmt.Sprintf("0.%d.%d.0", q.Sequence/65536, q.Sequence%65536)) {
		return nil, errors.New("Channel package version does not match sequence")
	}
	if err := r.assertPackages(channel, manifest); err != nil {
		return nil, err
	}
	var entry *ChannelPackage
	for i := range manifest.Packages {
		if manifest.Packages[i].Platform == r.platform && manifest.Packages[i].Arch == r.arch {
			entry = &manifest.Packages[i]
			break
		}
	}
	if entry == nil {
		return nil, errors.New("Channel has no package for this platform and architecture")
	}
	signer := entry.Publisher
	if entry.Platform == "darwin" {
		signer = entry.TeamID
	}
	if r.signer == "" || signer != r.signer {
		return nil, errors.New("Channel package signing identity mismatch")
	}
	return &ChannelTarget{Channel: channel, Manifest: manifest, Package: *entry, ManifestSHA256: channel.Head.SHA256,
		ArtifactURL: r.base + "/" + entry.Artifact.Key, FeedURL: r.base + "/" + entry.FeedKey}, nil
}

func (r *channelResolver) assertPackages(channel ChannelRecord, manifest ChannelManifest) error {
	q := manifest.Request
	prefix, err := buildPrefix(q.BuildID)
	if err != nil {
		return err
	}
	prefixes := []string{prefix}
	if channel.Policy != "preview" {
		if q.ReleaseTag == "" || q.Version != q.ReleaseTag[1:] {
			return errors.New("Protected release version mismatch")
		}
		ref := q.ArchiveRef
		if ref == "" {
			ref = q.ReleaseTag
		}
		prefixes = append(prefixes, "releases/tag/"+ref+"/")
	}
	under := func(key string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(key, p) {
				return true
			}
		}
		return false
	}
	for _, entry := range manifest.Packages {
		identity, version := q.Identity.MsixAppIDWithOrg, q.WindowsVersion
		if entry.Platform == "darwin" {
			identity, version = q.Identity.AppID, q.Version
		}
		if entry.Identity != identity || entry.Version != version || entry.FeedChannel != "stable" || !under(entry.Artifact.Key) || !under(entry.FeedKey) {
			return errors.New("Channel package identity, version or immutable prefix mismatch")
		}
		if entry.Platform == "darwin" && !strings.HasSuffix(entry.FeedKey, "/"+entry.FeedChannel+"-mac.yml") {
			return errors.New("Invalid native macOS feed descriptor")
		}
		if entry.Platform == "win32" && !strings.HasSuffix(entry.FeedKey, ".appinstaller") {
			return errors.New("Invalid native Windows feed descriptor")
		}
	}
	return nil
}

// What a channel check selected for the apply that follows it
type channelSelection struct {
	kind      string
	strategy  UpdaterStrategy
	available bool
}

// The channel build update owner, a native strategy pinned to the resolved channel head
type channelStrategy struct {
	build         *ChannelBuild
	mechanism     string
	resolver      *channelResolver
	nativeFactory func(*ChannelTarget) (UpdaterStrategy, error)

	mu        sync.Mutex
	busy      bool
	selection *channelSelection
}

func (c *channelStrategy) Mechanism() string { return c.mechanism }

func (c *channelStrategy) enter() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy {
		return errors.New("An update operation is already in progress.")
	}
	c.busy = true
	return nil
}

func (c *channelStrategy) leave() {
	c.mu.Lock()
	c.busy = false
	c.mu.Unlock()
}

func (c *channelStrategy) Check(bool) (UpdaterStatus, error) {
	if err := c.enter(); err != nil {
		return UpdaterStatus{}, err
	}
	defer c.leave()
	return c.selectTarget()
}

func (c *channelStrategy) selectTarget() (UpdaterStatus, error) {
	c.selection = nil
	result, err := c.resolver.resolve()
	if err != nil {
		return UpdaterStatus{}, err
	}
	base := UpdaterStatus{Supported: true, Mechanism: c.mechanism, Channel: c.build.Channel, CurrentVersion: c.build.Version, FetchedAt: time.Now().UnixMilli()}
	switch result.kind {
	case "empty":
		c.selection = &channelSelection{kind: "empty"}
		none := false
		base.UpdateAvailable, base.Reason = &none, "no-build-published"
		return base, nil
	case "retirement":
		if result.receiverKind == "in-place" {
			return c.selectNative(base, result.target, true)
		}
		c.selection = &channelSelection{kind: "retirement"}
		base.Retirement = &ChannelRetirementStatus{State: "discontinued", Destination: result.target.Channel.Name, Version: result.target.Manifest.Request.Version}
		return base, nil
	}
	return c.selectNative(base, result.target, false)
}

func (c *channelStrategy) selectNative(base UpdaterStatus, target *ChannelTarget, crossChannel bool) (UpdaterStatus, error) {
	if !crossChannel && target.Manifest.Request.Sequence <= c.build.Sequence {
		c.selection = &channelSelection{kind: "empty"}
		none := false
		base.UpdateAvailable = &none
		return base, nil
	}
	strategy, err := c.nativeFactory(target)
	if err != nil {
		return UpdaterStatus{}, err
	}
	status, err := strategy.Check(false)
	if err != nil {
		return UpdaterStatus{}, err
	}
	if status.Error != "" || status.UpdateAvailable == nil {
		return UpdaterStatus{}, errors.New(firstNonEmpty(status.Error, "Native update availability unknown"))
	}
	c.selection = &channelSelection{kind: "native", strategy: strategy, available: *status.UpdateAvailable}
	status.Supported, status.Mechanism, status.Channel, status.CurrentVersion, status.FetchedAt = base.Supported, base.Mechanism, base.Channel, base.CurrentVersion, base.FetchedAt
	latest := "v" + target.Manifest.Request.Version
	status.LatestTag, status.TargetSHA = &latest, target.Manifest.Request.Commit
	return status, nil
}

func (c *channelStrategy) Apply() (ApplyResult, error) {
	if err := c.enter(); err != nil {
		return ApplyResult{}, err
	}
	defer c.leave()
	if c.selection == nil {
		if _, err := c.selectTarget(); err != nil {
			return ApplyResult{}, err
		}
	}
	selected := c.selection
	if selected != nil && selected.kind == "retirement" {
		return ApplyResult{Error: "This build is discontinued; uninstall it and install an official release."}, nil
	}
	if selected != nil && selected.kind == "native" && selected.available {
		return selected.strategy.Apply()
	}
	return ApplyResult{OK: true, Mechanism: c.mechanism}, nil
}

// Channel platform name of this process as Node spells it
func channelPlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// Channel architecture of this process as Node spells it
func channelArch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}

// Verifies a downloaded channel artifact against its pinned size and sha256
func verifyChannelDownload(files []string, artifact ChannelArtifact) error {
	if len(files) != 1 {
		return errors.New("Expected exactly one channel artifact")
	}
	handle, err := os.Open(files[0])
	if err != nil {
		return err
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != artifact.Size {
		return errors.New("Channel artifact size mismatch")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, handle); err != nil {
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != artifact.SHA256 {
		return errors.New("Channel artifact digest mismatch")
	}
	return nil
}

// Runs a native channel command with stdin input, failing with its stderr
func runChannelCommand(file string, args []string, timeout time.Duration, input string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, file, args...)
	hideConsole(cmd)
	cmd.Stdin = strings.NewReader(input)
	stdout, stderr := &cappedBuffer{limit: channelReadLimit}, &cappedBuffer{limit: channelReadLimit}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("Native channel operation failed: %s: %s", file, stderr.String())
	}
	return stdout.String(), stderr.String(), nil
}

// Runs a PowerShell script encoded as UTF-16LE base64, Windows only
func runChannelPowerShell(script, input string) (string, string, error) {
	if runtime.GOOS != "windows" {
		return "", "", errors.New("This operation requires Windows")
	}
	units := utf16.Encode([]rune(script))
	encoded := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(encoded[i*2:], unit)
	}
	return runChannelCommand(filepath.Join(windowsSystemRoot(), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
		[]string{"-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(encoded)}, channelPowerShellLimit, input)
}

// Real path of an absolute path, the missing tail joined onto its nearest existing parent
func canonicalChannelPath(candidate string) (string, error) {
	if !filepath.IsAbs(candidate) || strings.Contains(candidate, "\x00") {
		return "", errors.New("Channel operations require absolute local paths")
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(candidate)
	if parent == candidate {
		return "", err
	}
	base, err := canonicalChannelPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, filepath.Base(candidate)), nil
}

// RunningChannelApp is the signed installed identity of the running channel app
type RunningChannelApp struct {
	Identity          string   `json:"identity"`
	Signer            string   `json:"signer"`
	AppPath           string   `json:"appPath"`
	NativeVersion     string   `json:"nativeVersion"`
	Executable        string   `json:"executable"`
	ApplicationID     *string  `json:"applicationId"`
	PackageFullName   *string  `json:"packageFullName"`
	PackageFamilyName *string  `json:"packageFamilyName"`
	RemovalRoots      []string `json:"removalRoots"`
}

var (
	teamIdentifierRE = regexp.MustCompile(`(?m)^TeamIdentifier=(.+)$`)
	identifierRE     = regexp.MustCompile(`(?m)^Identifier=(.+)$`)
)

const inspectPackageScript = `
$ErrorActionPreference='Stop'
$p=[Console]::In.ReadToEnd() | ConvertFrom-Json
$packages=@(Get-AppxPackage -Name $p.identity | Where-Object { $_.Name -ceq $p.identity })
if ($packages.Count -ne 1) { throw 'Running channel package is not uniquely registered' }
$pkg=$packages[0]
if ($pkg.Status.ToString() -cne 'Ok' -or $pkg.SignatureKind.ToString() -cne 'Developer' -or $pkg.IsDevelopmentMode -or $pkg.NonRemovable) { throw 'Channel package is not a trusted current-user sideload' }
if (-not $p.executable.StartsWith($pkg.InstallLocation.TrimEnd('\')+'\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Running executable is outside registered package' }
$manifest=Get-AppxPackageManifest -Package $pkg.PackageFullName
$apps=@($manifest.Package.Applications.Application | Where-Object { [IO.Path]::GetFullPath((Join-Path $pkg.InstallLocation $_.Executable)) -ieq $p.executable })
if ($apps.Count -ne 1) { throw 'Running executable is not the registered application' }
@{ identity=$pkg.Name; signer=$pkg.Publisher; appPath=$pkg.InstallLocation; nativeVersion=$pkg.Version.ToString(); executable=$p.executable; applicationId=$apps[0].Id; packageFullName=$pkg.PackageFullName; packageFamilyName=$pkg.PackageFamilyName; removalRoots=@($pkg.InstallLocation,(Join-Path ([Environment]::GetFolderPath('LocalApplicationData')) ('Packages\'+$pkg.PackageFamilyName))) } | ConvertTo-Json -Compress
`

// Signed identity of the running channel app, the code signature on macOS and the registered package on Windows
func (s *Service) inspectRunningChannelApp(build *ChannelBuild) (*RunningChannelApp, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	executable, err := canonicalChannelPath(exe)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "darwin" {
		appPath := filepath.Clean(filepath.Join(executable, "..", "..", ".."))
		if _, _, err := runChannelCommand("/usr/bin/codesign", []string{"--verify", "--deep", "--strict", appPath}, channelCommandTimeout, ""); err != nil {
			return nil, err
		}
		_, stderr, err := runChannelCommand("/usr/bin/codesign", []string{"-dv", "--verbose=4", appPath}, channelCommandTimeout, "")
		if err != nil {
			return nil, err
		}
		signer, identity := "", ""
		if m := teamIdentifierRE.FindStringSubmatch(stderr); m != nil {
			signer = m[1]
		}
		if m := identifierRE.FindStringSubmatch(stderr); m != nil {
			identity = m[1]
		}
		if !teamIDRE.MatchString(signer) || identity != build.Identity.AppID {
			return nil, errors.New("Running channel application signature does not match its baked identity")
		}
		return &RunningChannelApp{Identity: identity, Signer: signer, AppPath: appPath, Executable: executable, NativeVersion: build.Version, RemovalRoots: []string{appPath}}, nil
	}
	input, _ := json.Marshal(map[string]string{"identity": build.Identity.MsixAppIDWithOrg, "executable": executable})
	stdout, _, err := runChannelPowerShell(inspectPackageScript, string(input))
	if err != nil {
		return nil, err
	}
	var installed RunningChannelApp
	if err := json.Unmarshal([]byte(stdout), &installed); err != nil {
		return nil, err
	}
	if installed.NativeVersion != build.WindowsVersion || installed.Identity != build.Identity.MsixAppIDWithOrg || installed.Signer == "" {
		return nil, errors.New("Running package does not match its baked channel version")
	}
	return &installed, nil
}

// Verifies a pinned artifact's type, size and sha256
func verifyPinnedArtifact(file string, size int64, sum string) error {
	info, err := os.Lstat(file)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return errors.New("Pinned artifact size or type mismatch")
	}
	handle, err := os.Open(file)
	if err != nil {
		return err
	}
	defer handle.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, handle); err != nil {
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != sum {
		return errors.New("Pinned artifact digest mismatch")
	}
	return nil
}

// Downloads a pinned artifact into directory once, reusing a verified earlier download
func downloadPinnedArtifact(directory, rawURL, sum string, size int64, format string) (string, error) {
	file := filepath.Join(directory, "destination."+format)
	if err := verifyPinnedArtifact(file, size, sum); err == nil {
		return file, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	temporary := file + ".partial"
	if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pinnedArtifactTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := noRedirectClient(0).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("Pinned artifact download failed (%d)", resp.StatusCode)
	}
	handle, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	written, copyErr := io.Copy(handle, io.LimitReader(resp.Body, size+1))
	if copyErr == nil && written > size {
		copyErr = errors.New("Pinned artifact download exceeds pinned size")
	}
	if copyErr == nil {
		copyErr = handle.Sync()
	}
	if closeErr := handle.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return "", copyErr
	}
	if err := verifyPinnedArtifact(temporary, size, sum); err != nil {
		return "", err
	}
	return file, os.Rename(temporary, file)
}

const verifyInstallerScript = `
$ErrorActionPreference='Stop'
$p=[Console]::In.ReadToEnd() | ConvertFrom-Json
function Read-SafeXml($stream) {
  $settings=[Xml.XmlReaderSettings]::new(); $settings.DtdProcessing=[Xml.DtdProcessing]::Prohibit; $settings.XmlResolver=$null
  $reader=[Xml.XmlReader]::Create($stream,$settings)
  try { $xml=[Xml.XmlDocument]::new(); $xml.XmlResolver=$null; $xml.Load($reader); return ,$xml } finally { $reader.Dispose() }
}
$stream=[IO.File]::OpenRead($p.file)
try { $xml=Read-SafeXml $stream } finally { $stream.Dispose() }
$root=$xml.DocumentElement
if ($root.LocalName -cne 'AppInstaller' -or $root.GetAttribute('Uri') -cne $p.feedUrl -or $root.GetAttribute('Version') -cne $p.version) { throw 'Channel descriptor binding mismatch' }
$main=@($root.ChildNodes | Where-Object { $_.LocalName -in @('MainPackage','MainBundle') })
if ($main.Count -ne 1 -or $main[0].GetAttribute('Name') -cne $p.identity -or $main[0].GetAttribute('Publisher') -cne $p.publisher -or $main[0].GetAttribute('Version') -cne $p.version -or $main[0].GetAttribute('Uri') -cne $p.artifactUrl) { throw 'Descriptor does not reference the pinned native package' }
if (@($root.ChildNodes | Where-Object { $_.LocalName -notin @('MainPackage','MainBundle') }).Count) { throw 'Pinned channel descriptor must not add dependencies, optional packages or background update policies' }
$signature=Get-AuthenticodeSignature -LiteralPath $p.artifact
if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -cne $p.publisher) { throw 'Native artifact signature/publisher mismatch' }
Add-Type -AssemblyName System.IO.Compression.FileSystem
$zip=[IO.Compression.ZipFile]::OpenRead($p.artifact)
try {
  $name=if ($p.artifact.EndsWith('.msixbundle')) { 'AppxMetadata/AppxBundleManifest.xml' } else { 'AppxManifest.xml' }
  $entry=$zip.GetEntry($name)
  if (-not $entry) { throw 'Native manifest missing' }
  $stream=$entry.Open()
  try { $native=Read-SafeXml $stream } finally { $stream.Dispose() }
  if ($name -ceq 'AppxManifest.xml') { $identity=$native.Package.Identity; if ($identity.ProcessorArchitecture -cne $p.arch) { throw 'Package architecture mismatch' } }
  else { $identity=$native.Bundle.Identity; $slices=@($native.Bundle.Packages.Package | Where-Object { $_.Type -ceq 'application' -and $_.Architecture -ceq $p.arch -and $_.Version -ceq $p.version }); if ($slices.Count -ne 1) { throw 'Bundle architecture/version mismatch' } }
  if ($identity.Name -cne $p.identity -or $identity.Publisher -cne $p.publisher -or $identity.Version -cne $p.version) { throw 'Native manifest binding mismatch' }
} finally { $zip.Dispose() }
`

// Verifies a prepared App Installer descriptor names exactly the pinned, signed native package
func (s *Service) verifyPreparedChannelInstaller(file string, target *ChannelTarget) error {
	pkg := target.Package
	if pkg.Platform != "win32" || pkg.Publisher == "" {
		return errors.New("Expected a publisher-bound Windows package")
	}
	directory, err := os.MkdirTemp(filepath.Dir(file), ".channel-artifact-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	format := "msix"
	if strings.HasSuffix(pkg.Artifact.Key, ".msixbundle") {
		format = "msixbundle"
	}
	artifact, err := downloadPinnedArtifact(directory, target.ArtifactURL, pkg.Artifact.SHA256, pkg.Artifact.Size, format)
	if err != nil {
		return err
	}
	input, _ := json.Marshal(map[string]string{"file": file, "artifact": artifact, "feedUrl": target.FeedURL, "artifactUrl": target.ArtifactURL,
		"identity": pkg.Identity, "publisher": pkg.Publisher, "version": pkg.Version, "arch": pkg.Arch})
	_, _, err = runChannelPowerShell(verifyInstallerScript, string(input))
	return err
}
