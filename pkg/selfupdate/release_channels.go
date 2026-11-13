package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	channelMaxSequence = 1<<32 - 1
	channelMaxMetadata = 4 * 1024 * 1024
	channelMaxSafeInt  = 1<<53 - 1
)

var channelPolicies = map[string]bool{"preview": true, "stable-release": true, "canary-release": true, "source-branch": true}

var (
	channelRepositoryRE    = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9_-]*/[A-Za-z0-9][A-Za-z0-9_.-]*)$`)
	channelSHA256RE        = regexp.MustCompile(`^[a-f0-9]{64}$`)
	channelCommitRE        = regexp.MustCompile(`^[a-f0-9]{40}$`)
	channelBuildIDRE       = regexp.MustCompile(`^[a-f0-9]{32}$`)
	channelSourceVersionRE = regexp.MustCompile(`^(?:(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))$`)
	channelReleaseTagRE    = regexp.MustCompile(`^(?:v[0-9]+\.[0-9]+\.[0-9]+(?:\+canary\.20[0-9]{6}T[0-9]{6}Z)?)$`)
	channelCanaryQuadRE    = regexp.MustCompile(`^(?:[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)$`)
	channelStableQuadRE    = regexp.MustCompile(`^(?:[0-9]+\.[0-9]+\.[0-9]+\.0)$`)
	channelTokenRE         = regexp.MustCompile(`^[a-f0-9]{16}$`)
	channelDisplayRE       = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9 ._-]{0,79})$`)
	channelPascalRE        = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9]{0,63})$`)
	channelAppIDRE         = regexp.MustCompile(`^(?:[a-z][a-z0-9.-]{2,127})$`)
	channelMSIXRE          = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9.-]{2,49})$`)
	channelCLIRE           = regexp.MustCompile(`^(?:[a-z][a-z0-9-]{0,63})$`)
	channelEnvNameRE       = regexp.MustCompile(`^(?:[A-Za-z_][A-Za-z0-9_]*)$`)
	channelBranchRE        = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9_./-]*)$`)
	channelArtifactRE      = regexp.MustCompile(`^[A-Za-z0-9_./+-]+$`)
	channelTeamRE          = regexp.MustCompile(`^[A-Z0-9]{10}$`)
	attemptRefRE           = regexp.MustCompile(`^rc\.([1-9][0-9]*)-(v[^/]+)$`)
)

// ChannelNotFound is the missing channel object, still a ChannelError to every caller
type ChannelNotFound struct{ ChannelError }

func (e *ChannelNotFound) Unwrap() error { return &e.ChannelError }

func channelErr(msg string) error { return &ChannelError{Msg: msg} }

// String value when pattern matches all of it, the labelled ChannelError otherwise
func channelMatch(pattern *regexp.Regexp, value any, label string) (string, error) {
	s, ok := value.(string)
	if !ok || !pattern.MatchString(s) {
		return "", channelErr("Invalid " + label)
	}
	return s, nil
}

func validateChannelRepository(value any) (string, error) {
	return channelMatch(channelRepositoryRE, value, "repository")
}

func requireChannelSHA256(value any) (string, error) {
	return channelMatch(channelSHA256RE, value, "SHA256")
}

func requireChannelCommit(value any) (string, error) {
	return channelMatch(channelCommitRE, value, "commit")
}

func channelBuildPrefix(buildID any) (string, error) {
	id, err := channelMatch(channelBuildIDRE, buildID, "build ID")
	if err != nil {
		return "", err
	}
	return "releases/channel-builds/" + id + "/", nil
}

func channelKey(name any) (string, error) {
	valid, err := ValidateChannelName(name)
	if err != nil {
		return "", err
	}
	return "releases/channels/" + valid + ".json", nil
}

// Sorted compact ASCII JSON with a trailing newline, the byte form every channel digest covers
func canonicalJSON(value any) ([]byte, error) {
	text, err := pyjson.Dumps(value, pyjson.SortKeys(true), pyjson.Separators(",", ":"), pyjson.EnsureASCII(true), pyjson.AllowNaN(false))
	if err != nil {
		return nil, err
	}
	return []byte(text + "\n"), nil
}

// Channel object decoded with duplicate keys refused and a top level object required
func decodeChannelJSON(body []byte) (*pyjson.Object, error) {
	value, err := pyjson.LoadsBytesPairsHook(body, func(keys []string, vals []any) (any, error) {
		obj := pyjson.NewObject()
		for i, key := range keys {
			if _, dup := obj.Vals[key]; dup {
				return nil, channelErr("Duplicate JSON key")
			}
			obj.Set(key, vals[i])
		}
		return obj, nil
	})
	if err != nil {
		return nil, channelErr("Invalid channel JSON")
	}
	obj, ok := value.(*pyjson.Object)
	if !ok {
		return nil, channelErr("Channel metadata must be an object")
	}
	return obj, nil
}

// Archive base without trailing slashes when it is a plain https or loopback http authority
func channelPublicBase(value string) (string, error) {
	parsed, err := pyurl.Split(value)
	if err != nil {
		return "", &ValueError{Msg: err.Error()}
	}
	host := parsed.Hostname()
	bad := host == "" || strings.Contains(parsed.Netloc, "@") || parsed.Query != "" || parsed.Fragment != "" ||
		strings.ContainsAny(value, "%\\;")
	for _, c := range value {
		if c <= 32 {
			bad = true
		}
	}
	for _, part := range strings.Split(parsed.Path, "/") {
		if part == "." || part == ".." {
			bad = true
		}
	}
	loopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
	if bad || !(parsed.Scheme == "https" || (parsed.Scheme == "http" && loopback)) {
		return "", channelErr("Invalid channel archive authority")
	}
	return strings.TrimRight(value, "/"), nil
}

// Release object key under releases/ with no empty, dot or reserved device segment
func channelArtifactKey(value any) (string, error) {
	key, err := channelMatch(channelArtifactRE, value, "artifact key")
	if err != nil {
		return "", err
	}
	parts := strings.Split(key, "/")
	if !strings.HasPrefix(key, "releases/") {
		return "", channelErr("Invalid artifact key")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", channelErr("Invalid artifact key")
		}
	}
	for _, part := range parts {
		if reservedChannelNames[strings.ToLower(strings.SplitN(part, ".", 2)[0])] || strings.HasSuffix(part, ".") {
			return "", channelErr("Invalid artifact key")
		}
	}
	return key, nil
}

// Package version and Windows quad a sequence publishes, the quad carrying the sequence in its middle fields
func channelPackageVersions(sequence any) (string, string, error) {
	n, err := channelInteger(sequence, "sequence", 1, channelMaxSequence)
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("0.0.%d", n), fmt.Sprintf("0.%d.%d.0", n/65536, n%65536), nil
}

// Python int value in range, a bool or any other type answering the labelled ChannelError
func channelInteger(value any, label string, minimum, maximum int64) (int64, error) {
	var n *big.Int
	switch v := value.(type) {
	case int64:
		n = big.NewInt(v)
	case *big.Int:
		n = v
	}
	if n == nil || n.Cmp(big.NewInt(minimum)) < 0 || n.Cmp(big.NewInt(maximum)) > 0 {
		return 0, channelErr("Invalid " + label)
	}
	return n.Int64(), nil
}

func channelSchema(value any) (*pyjson.Object, error) {
	obj, ok := value.(*pyjson.Object)
	if !ok {
		return nil, channelErr("Unsupported channel schema")
	}
	if schema, isInt := obj.Vals["schema"].(int64); !isInt || schema != 1 {
		return nil, channelErr("Unsupported channel schema")
	}
	return obj, nil
}

// Identity object whose token, names, app ids and executable names all match their wire grammar
func validateChannelIdentity(value any) (*pyjson.Object, error) {
	obj, ok := value.(*pyjson.Object)
	if !ok {
		return nil, channelErr("Invalid channel identity")
	}
	checks := []struct {
		re           *regexp.Regexp
		field, label string
	}{
		{channelTokenRE, "token", "identity token"},
		{channelDisplayRE, "displayName", "identity display name"},
		{channelPascalRE, "appNamePascal", "appNamePascal"},
		{channelPascalRE, "artifactNamePascal", "artifactNamePascal"},
		{channelAppIDRE, "appId", "app ID"},
		{channelMSIXRE, "msixAppIdWithOrg", "MSIX identity"},
		{channelCLIRE, "cliName", "CLI name"},
		{channelDisplayRE, "windowsExecutableName", "Windows executable"},
	}
	for _, check := range checks {
		if _, err := channelMatch(check.re, obj.Vals[check.field], check.label); err != nil {
			return nil, err
		}
	}
	return obj, nil
}

// Version a final vX.Y.Z attempt ref names, false for any other ref shape
func parseAttemptRef(ref string) (string, bool) {
	m := attemptRefRE.FindStringSubmatch(ref)
	if m == nil || !updaterecord.StableTagRE.MatchString(m[2]) {
		return "", false
	}
	return m[2][1:], true
}

// Build request checked against its policy, repository and archive authority
func validateChannelRequest(value any, repository, baseURL *string, policy string) (*pyjson.Object, error) {
	request, err := channelSchema(value)
	if err != nil {
		return nil, err
	}
	v := request.Vals
	if _, err := channelBuildPrefix(v["buildId"]); err != nil {
		return nil, err
	}
	if _, err := ValidateChannelName(v["channel"]); err != nil {
		return nil, err
	}
	actualRepo, err := validateChannelRepository(v["repository"])
	if err != nil {
		return nil, err
	}
	if repository != nil && !strings.EqualFold(actualRepo, *repository) {
		return nil, channelErr("Channel repository authority mismatch")
	}
	if _, err := requireChannelCommit(v["commit"]); err != nil {
		return nil, err
	}
	if controller, ok := v["controllerCommit"]; ok {
		if _, err := requireChannelCommit(controller); err != nil {
			return nil, err
		}
	}
	if _, err := channelMatch(channelSourceVersionRE, v["sourceVersion"], "source version"); err != nil {
		return nil, err
	}
	version, windowsVersion, err := channelPackageVersions(v["sequence"])
	if err != nil {
		return nil, err
	}
	if policy == "preview" {
		if !pyjson.Equal(v["version"], version) || !pyjson.Equal(v["windowsVersion"], windowsVersion) {
			return nil, channelErr("Channel package version does not match sequence")
		}
	} else {
		tag, err := channelMatch(channelReleaseTagRE, v["releaseTag"], "release tag")
		if err != nil {
			return nil, err
		}
		if !pyjson.Equal(v["version"], tag[1:]) {
			return nil, channelErr("Release package version mismatch")
		}
		if archiveRef, present := v["archiveRef"]; present && archiveRef != nil {
			ref, isStr := archiveRef.(string)
			parsed, ok := "", false
			if isStr {
				parsed, ok = parseAttemptRef(ref)
			}
			if !ok {
				if !isStr || ref != tag {
					return nil, channelErr("Invalid archive ref")
				}
			} else if !pyjson.Equal(parsed, v["version"]) {
				return nil, channelErr("Archive ref does not name the release version")
			}
		}
		pattern := channelStableQuadRE
		if policy == "canary-release" {
			pattern = channelCanaryQuadRE
		}
		quad, err := channelMatch(pattern, v["windowsVersion"], "Windows version")
		if err != nil {
			return nil, err
		}
		for _, part := range strings.Split(quad, ".") {
			if n, _ := new(big.Int).SetString(part, 10); n.Cmp(big.NewInt(65535)) > 0 {
				return nil, channelErr("Invalid Windows version")
			}
		}
	}
	if _, err := validateChannelIdentity(v["identity"]); err != nil {
		return nil, err
	}
	values, ok := v["bundleEnv"].(*pyjson.Object)
	if !ok {
		return nil, channelErr("Invalid bundle environment")
	}
	for _, key := range values.Keys {
		if _, err := channelMatch(channelEnvNameRE, key, "bundle environment name"); err != nil {
			return nil, err
		}
		item := values.Vals[key]
		if s, isStr := item.(string); item != nil && (!isStr || strings.Contains(s, "\x00")) {
			return nil, channelErr("Invalid bundle environment value")
		}
	}
	base, isStr := v["publicBase"].(string)
	if !isStr {
		return nil, channelErr("Invalid request archive authority")
	}
	normalized, err := channelPublicBase(base)
	if err != nil {
		return nil, err
	}
	if normalized != base {
		return nil, channelErr("Invalid request archive authority")
	}
	if baseURL != nil {
		admitted, err := channelPublicBase(*baseURL)
		if err != nil {
			return nil, err
		}
		if base != admitted {
			return nil, channelErr("Request archive authority mismatch")
		}
	}
	return request, nil
}

// Validated channel head, nil for an unpublished channel
func channelHead(value any) (*pyjson.Object, error) {
	if value == nil {
		return nil, nil
	}
	head, ok := value.(*pyjson.Object)
	if !ok {
		return nil, channelErr("Invalid channel head")
	}
	prefix, err := channelBuildPrefix(head.Vals["buildId"])
	if err != nil {
		return nil, err
	}
	if _, err := channelInteger(head.Vals["sequence"], "head sequence", 1, channelMaxSequence); err != nil {
		return nil, err
	}
	if !pyjson.Equal(head.Vals["manifestKey"], prefix+"build.json") {
		return nil, channelErr("Invalid channel manifest key")
	}
	if _, err := requireChannelSHA256(head.Vals["sha256"]); err != nil {
		return nil, err
	}
	return head, nil
}

func headSequence(head *pyjson.Object) int64 {
	n, _ := channelInteger(head.Vals["sequence"], "head sequence", 1, channelMaxSequence)
	return n
}

// Channel record checked for its fields, authority, policy, head and retirement constraints
func validateChannelRecord(value any, name, repository *string) (*pyjson.Object, error) {
	record, err := channelSchema(value)
	if err != nil {
		return nil, err
	}
	v := record.Vals
	for _, field := range []string{"name", "repository", "policy", "state", "revision", "nextSequence", "identity", "head"} {
		if _, ok := v[field]; !ok {
			return nil, channelErr("Missing channel record fields")
		}
	}
	if _, err := ValidateChannelName(v["name"]); err != nil {
		return nil, err
	}
	if name != nil && !pyjson.Equal(v["name"], *name) {
		return nil, channelErr("Channel name does not match object key")
	}
	actualRepo, err := validateChannelRepository(v["repository"])
	if err != nil {
		return nil, err
	}
	if repository != nil && !strings.EqualFold(actualRepo, *repository) {
		return nil, channelErr("Channel repository authority mismatch")
	}
	policy, _ := v["policy"].(string)
	state, _ := v["state"].(string)
	if !channelPolicies[policy] || (state != "active" && state != "retired") {
		return nil, channelErr("Invalid channel policy or state")
	}
	if _, err := channelInteger(v["revision"], "channel revision", 1, channelMaxSafeInt); err != nil {
		return nil, err
	}
	next, err := channelInteger(v["nextSequence"], "next sequence", 1, channelMaxSequence+1)
	if err != nil {
		return nil, err
	}
	head, err := channelHead(v["head"])
	if err != nil {
		return nil, err
	}
	if head != nil && headSequence(head) >= next {
		return nil, channelErr("Channel head exceeds allocation high-water")
	}
	if policy == "source-branch" {
		delivery, isObj := v["delivery"].(*pyjson.Object)
		if v["identity"] != nil || head != nil || !isObj || !pyjson.Equal(delivery.Vals["kind"], "source-branch") {
			return nil, channelErr("Source branch cannot carry native artifacts")
		}
		branch, err := channelMatch(channelBranchRE, delivery.Vals["branch"], "source branch")
		if err != nil {
			return nil, err
		}
		if strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.HasSuffix(branch, "/") ||
			strings.HasSuffix(branch, ".") || strings.HasSuffix(branch, ".lock") {
			return nil, channelErr("Invalid source branch")
		}
	} else if _, err := validateChannelIdentity(v["identity"]); err != nil {
		return nil, err
	}
	if state == "retired" {
		if _, err := ValidateChannelName(v["destination"]); err != nil {
			return nil, err
		}
		if _, err := channelMatch(channelSourceVersionRE, v["minimumVersion"], "retirement minimum version"); err != nil {
			return nil, err
		}
		destination, err := channelHead(v["destinationHead"])
		if err != nil {
			return nil, err
		}
		if destination == nil {
			return nil, channelErr("Retirement requires a pinned destination head")
		}
		if protocol, isInt := v["receiverProtocol"].(int64); !isInt || protocol != 1 {
			return nil, channelErr("Unsupported retirement receiver protocol")
		}
		receiver, isObj := v["receiver"].(*pyjson.Object)
		if !isObj || !(pyjson.Equal(receiver.Vals["kind"], "in-place") || pyjson.Equal(receiver.Vals["kind"], "discontinued")) {
			return nil, channelErr("Invalid retirement receiver kind")
		}
		last, err := channelHead(v["lastHead"])
		if err != nil {
			return nil, err
		}
		if (last == nil) != (head == nil) || (last != nil && !pyjson.Equal(last, head)) {
			return nil, channelErr("Retirement last head mismatch")
		}
	}
	return record, nil
}

// Requested record, the record it resolves to, and that record's verified build manifest
type ChannelResolution struct {
	Requested, Terminal, Manifest *pyjson.Object
}

// ChannelOpener performs one channel archive request, the urllib opener seam
type ChannelOpener func(req *http.Request) (*http.Response, error)

type retryingReadsKey struct{}

// Lets channel reads under ctx wait out a CDN blip with the pm bounded retries, explicit updates only
func WithRetryingReads(ctx context.Context) context.Context {
	return context.WithValue(ctx, retryingReadsKey{}, true)
}

var channelRetryWait func(time.Duration) error

// ChannelReader reads channel objects over HTTPS from one archive authority with digest checks
type ChannelReader struct {
	baseURL    string
	repository *string
	opener     ChannelOpener
}

// Reader for an archive base and an optional repository authority, refusing redirects
func NewChannelReader(baseURL string, repository *string, opener ChannelOpener) (*ChannelReader, error) {
	base, err := channelPublicBase(baseURL)
	if err != nil {
		return nil, err
	}
	r := &ChannelReader{baseURL: base, opener: opener}
	if repository != nil {
		repo, err := validateChannelRepository(*repository)
		if err != nil {
			return nil, err
		}
		r.repository = &repo
	}
	if r.opener == nil {
		client := &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{Proxy: sourceUrlopenProxy},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return channelErr("Channel archive redirects are not permitted")
			},
		}
		r.opener = client.Do
	}
	return r, nil
}

// Bytes of one channel object, size bounded and matched to sha256 when one is pinned
func (r *ChannelReader) ReadBytes(ctx context.Context, key string, sha *string) ([]byte, error) {
	artifact, err := channelArtifactKey(key)
	if err != nil {
		return nil, err
	}
	target := r.baseURL + "/" + artifact
	if sha != nil {
		if _, err := requireChannelSHA256(*sha); err != nil {
			return nil, err
		}
	}
	read := func() ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Cache-Control", "no-cache")
		resp, err := r.opener(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			reason := http.StatusText(resp.StatusCode)
			if _, text, ok := strings.Cut(resp.Status, " "); ok && text != "" {
				reason = text
			}
			return nil, &pm.HTTPError{Code: resp.StatusCode, Reason: reason, Header: resp.Header, URL: target}
		}
		final := target
		if resp.Request != nil && resp.Request.URL != nil {
			final = resp.Request.URL.String()
		}
		if final != target {
			return nil, channelErr("Channel archive redirects are not permitted")
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, channelMaxMetadata+1))
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, &pm.IncompleteReadError{Partial: int64(len(body)), Expected: resp.ContentLength}
		}
		return body, err
	}
	var body []byte
	if retrying, _ := ctx.Value(retryingReadsKey{}).(bool); retrying {
		body, err = pm.RetryNetwork(read, pm.RetryOptions{Wait: channelRetryWait})
	} else {
		body, err = read()
	}
	if err != nil {
		var httpErr *pm.HTTPError
		var chErr *ChannelError
		var incomplete *pm.IncompleteReadError
		switch {
		case errors.As(err, &httpErr):
			if httpErr.Code == 404 {
				return nil, &ChannelNotFound{ChannelError{Msg: "Channel object not found: " + key}}
			}
			msg := fmt.Sprintf("Channel read unavailable: HTTP %d", httpErr.Code)
			if block := pm.EdgeBlock(httpErr); block != "" {
				msg += "; " + block
			}
			return nil, channelErr(msg)
		case errors.As(err, &chErr):
			return nil, chErr
		case errors.As(err, &incomplete):
			return nil, incomplete
		}
		return nil, channelErr("Channel read unavailable")
	}
	if len(body) > channelMaxMetadata {
		return nil, channelErr("Channel metadata exceeds size limit")
	}
	if sha != nil {
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != *sha {
			return nil, channelErr("Channel metadata SHA256 mismatch")
		}
	}
	return body, nil
}

// Validated record of one named channel under the reader's repository authority
func (r *ChannelReader) ReadRecord(ctx context.Context, name string) (*pyjson.Object, error) {
	key, err := channelKey(name)
	if err != nil {
		return nil, err
	}
	body, err := r.ReadBytes(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	value, err := decodeChannelJSON(body)
	if err != nil {
		return nil, err
	}
	return validateChannelRecord(value, &name, r.repository)
}

// Requested and terminal records with the verified manifest, a retirement pinned to its qualified destination
func (r *ChannelReader) Resolve(ctx context.Context, name string) (*ChannelResolution, error) {
	requested, err := r.ReadRecord(ctx, name)
	if err != nil {
		return nil, err
	}
	record := requested
	retired := requested.Vals["state"] == "retired"
	if retired {
		destination := requested.Vals["destination"].(string)
		if destination == name {
			return nil, channelErr("Channel retirement cycle")
		}
		if record, err = r.ReadRecord(ctx, destination); err != nil {
			return nil, err
		}
		if !strings.EqualFold(record.Vals["repository"].(string), requested.Vals["repository"].(string)) {
			return nil, channelErr("Retirement repository authority mismatch")
		}
		if record.Vals["state"] != "active" || record.Vals["policy"] != "stable-release" {
			return nil, channelErr("Retirement requires a directly active stable-release destination")
		}
	}
	head, _ := record.Vals["head"].(*pyjson.Object)
	if retired {
		head, _ = requested.Vals["destinationHead"].(*pyjson.Object)
		current, _ := record.Vals["head"].(*pyjson.Object)
		if head == nil || current == nil || headSequence(head) > headSequence(current) {
			return nil, channelErr("Invalid retirement destination head")
		}
	}
	var manifest *pyjson.Object
	if head != nil {
		sha := head.Vals["sha256"].(string)
		body, err := r.ReadBytes(ctx, head.Vals["manifestKey"].(string), &sha)
		if err != nil {
			return nil, err
		}
		value, err := decodeChannelJSON(body)
		if err != nil {
			return nil, err
		}
		pinned := pyjson.Clone(record).(*pyjson.Object)
		pinned.Set("head", head)
		if manifest, err = validateChannelManifest(value, pinned, r.baseURL); err != nil {
			return nil, err
		}
	}
	if retired {
		if !pyjson.Equal(manifest.Vals["receiverProtocol"], requested.Vals["receiverProtocol"]) {
			return nil, channelErr("Stable build has no supported retirement receiver")
		}
		actual := manifest.Vals["request"].(*pyjson.Object).Vals["sourceVersion"].(string)
		if compareDottedInts(actual, requested.Vals["minimumVersion"].(string)) < 0 {
			return nil, channelErr("Destination does not meet retirement minimum version")
		}
	}
	return &ChannelResolution{Requested: requested, Terminal: record, Manifest: manifest}, nil
}

// Orders two dotted decimal versions as Python orders their int tuples
func compareDottedInts(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(left) && i < len(right); i++ {
		x, _ := new(big.Int).SetString(left[i], 10)
		y, _ := new(big.Int).SetString(right[i], 10)
		if c := x.Cmp(y); c != 0 {
			return c
		}
	}
	return len(left) - len(right)
}

// Build manifest bound to its record's identity, head and admitted archive prefixes
func validateChannelManifest(value any, record *pyjson.Object, baseURL string) (*pyjson.Object, error) {
	manifest, err := channelSchema(value)
	if err != nil {
		return nil, err
	}
	if protocol, ok := manifest.Vals["receiverProtocol"]; ok {
		if _, err := channelInteger(protocol, "receiver protocol", 1, channelMaxSafeInt); err != nil {
			return nil, err
		}
	}
	repository, _ := record.Vals["repository"].(string)
	policy, _ := record.Vals["policy"].(string)
	request, err := validateChannelRequest(manifest.Vals["request"], &repository, &baseURL, policy)
	if err != nil {
		return nil, err
	}
	rv := request.Vals
	if !pyjson.Equal(rv["identity"], record.Vals["identity"]) {
		return nil, channelErr("Manifest channel identity mismatch")
	}
	if !pyjson.Equal(rv["channel"], record.Vals["name"]) {
		return nil, channelErr("Manifest channel mismatch")
	}
	if head, ok := record.Vals["head"].(*pyjson.Object); ok {
		for _, field := range []string{"buildId", "sequence"} {
			if !pyjson.Equal(rv[field], head.Vals[field]) {
				return nil, channelErr("Manifest does not match channel head")
			}
		}
	}
	packages, ok := manifest.Vals["packages"].([]any)
	if !ok || len(packages) == 0 {
		return nil, channelErr("Build manifest has no packages")
	}
	prefix, _ := channelBuildPrefix(rv["buildId"])
	prefixes := []string{prefix}
	if policy == "stable-release" || policy == "canary-release" {
		archive, _ := rv["archiveRef"].(string)
		if archive == "" {
			archive = rv["releaseTag"].(string)
		}
		prefixes = append(prefixes, "releases/tag/"+archive+"/")
	}
	admitted := func(key string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(key, p) {
				return true
			}
		}
		return false
	}
	identity := rv["identity"].(*pyjson.Object)
	seen := map[[3]string]bool{}
	for _, entry := range packages {
		pkg, ok := entry.(*pyjson.Object)
		if !ok {
			return nil, channelErr("Invalid native package")
		}
		platform, _ := pkg.Vals["platform"].(string)
		arch, _ := pkg.Vals["arch"].(string)
		variant, _ := pkg.Vals["variant"].(string)
		if (platform != "darwin" && platform != "win32") || (arch != "arm64" && arch != "x64") || variant != "bundled" {
			return nil, channelErr("Unsupported channel package target")
		}
		target := [3]string{platform, arch, variant}
		if seen[target] {
			return nil, channelErr("Duplicate package target")
		}
		seen[target] = true
		artifact, ok := pkg.Vals["artifact"].(*pyjson.Object)
		if !ok {
			return nil, channelErr("Invalid package artifact")
		}
		key, err := channelArtifactKey(artifact.Vals["key"])
		if err != nil {
			return nil, err
		}
		if !admitted(key) {
			return nil, channelErr("Artifact outside admitted build namespace")
		}
		if _, err := requireChannelSHA256(artifact.Vals["sha256"]); err != nil {
			return nil, err
		}
		if _, err := channelInteger(artifact.Vals["size"], "artifact size", 1, channelMaxSafeInt); err != nil {
			return nil, err
		}
		identityField, versionField, signingField := "msixAppIdWithOrg", "windowsVersion", "publisher"
		if platform == "darwin" {
			identityField, versionField, signingField = "appId", "version", "teamId"
		}
		if !pyjson.Equal(pkg.Vals["identity"], identity.Vals[identityField]) || !pyjson.Equal(pkg.Vals["version"], rv[versionField]) {
			return nil, channelErr("Native package identity or version mismatch")
		}
		signing, isStr := pkg.Vals[signingField].(string)
		control := false
		for _, c := range signing {
			if c < 32 {
				control = true
			}
		}
		if !isStr || signing == "" || control {
			return nil, channelErr("Missing native signing identity")
		}
		if platform == "darwin" {
			if _, err := channelMatch(channelTeamRE, signing, "Apple team ID"); err != nil {
				return nil, err
			}
		}
		feed, ok := pkg.Vals["feed"].(*pyjson.Object)
		if !ok || !pyjson.Equal(feed.Vals["channel"], "stable") {
			return nil, channelErr("Channel native metadata requires a neutral stable feed")
		}
		feedKey, err := channelArtifactKey(feed.Vals["key"])
		if err != nil {
			return nil, err
		}
		if !admitted(feedKey) {
			return nil, channelErr("Native metadata outside admitted build namespace")
		}
	}
	return manifest, nil
}
