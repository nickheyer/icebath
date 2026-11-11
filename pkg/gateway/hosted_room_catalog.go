package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"math/big"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyipaddress"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Room protocol version this gateway speaks
const HostedProtocolVersion = 2

var hostedLinkModes = map[string]bool{"direct": true, "overlay": true, "relay": true, "pull": true, "desktop": true}

var hostedCatalogFields = map[string]bool{
	"installation_id": true, "protocol_versions": true, "link_modes": true, "persistent_process": true, "text": true,
	"attachments": true, "execution_policy": true, "catalog_digest": true}

func hostedPeerError(msg string) error { return &HostedRoomPeerError{msg} }

// Exact peer fields with the hosted_room_peer message formats
func hostedPeerExactFields(value any, label string, required, optional map[string]bool) (map[string]any, error) {
	return roomExactFields(value, label, required, optional, hostedPeerError,
		roomFieldFormats{missing: "{label} missing fields: {fields}", unknown: "{label} unknown fields: {fields}"})
}

// Non empty list of a peer field
func hostedNonEmptyList(value any, field string) ([]any, error) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil, &HostedRoomPeerError{field + " must be a non-empty list"}
	}
	return items, nil
}

// Supported link modes of a peer list, deduplicated in first seen order
func hostedParseLinkModes(raw any) ([]string, error) {
	items, err := hostedNonEmptyList(raw, "link_modes")
	if err != nil {
		return nil, err
	}
	var modes []string
	seen := map[string]bool{}
	for _, item := range items {
		switch item.(type) {
		case []any, map[string]any, *pyjson.Object:
			return nil, &PyError{Kind: "TypeError", Msg: "unhashable type: '" + pyTypeName(item) + "'"}
		}
		mode, ok := item.(string)
		if !ok || !hostedLinkModes[mode] {
			return nil, &HostedRoomPeerError{"link_modes contains an unsupported mode"}
		}
		if !seen[mode] {
			seen[mode] = true
			modes = append(modes, mode)
		}
	}
	return modes, nil
}

// Sorted distinct positive protocol versions
func hostedProtocolVersions(raw []any) ([]*big.Int, error) {
	var versions []*big.Int
	seen := map[string]bool{}
	for _, item := range raw {
		n, err := hostedPositiveInt(item, "protocol_version")
		if err != nil {
			return nil, err
		}
		if !seen[n.String()] {
			seen[n.String()] = true
			versions = append(versions, n)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Cmp(versions[j]) < 0 })
	return versions, nil
}

// Digest of a catalog mapping without its catalog_digest field
func hostedCatalogDigest(value *pyjson.Object) (string, error) {
	unsigned := pyjson.NewObject()
	for _, key := range value.Keys {
		if key != "catalog_digest" {
			item, _ := value.Get(key)
			unsigned.Set(key, item)
		}
	}
	text, err := roomCompactJSON(unsigned, true)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:]), nil
}

// Advertised endpoint capability as its URL, unavailability reason and transport security
type hostedEndpoint struct {
	url, reason, transportSecurity *string
}

// Endpoint capability validated from its peer mapping
func hostedParseEndpoint(endpoint any) (hostedEndpoint, error) {
	fields, ok := hostedMapping(endpoint)
	available, isBool := fields["available"].(bool)
	if !ok || !isBool {
		return hostedEndpoint{}, &HostedRoomPeerError{"endpoint capability is invalid"}
	}
	if !available {
		if _, err := hostedPeerExactFields(endpoint, "endpoint capability", map[string]bool{"available": true, "reason": true}, nil); err != nil {
			return hostedEndpoint{}, err
		}
		reason, err := hostedIdentifier(fields["reason"], "endpoint.reason")
		if err != nil {
			return hostedEndpoint{}, err
		}
		return hostedEndpoint{reason: &reason}, nil
	}
	if _, err := hostedPeerExactFields(endpoint, "endpoint capability", map[string]bool{"available": true, "url": true, "transport_security": true}, nil); err != nil {
		return hostedEndpoint{}, err
	}
	url, security, err := ValidateRoomLinkURL(fields["url"])
	if err != nil {
		return hostedEndpoint{}, err
	}
	if !pyjson.Equal(fields["transport_security"], security) {
		return hostedEndpoint{}, &HostedRoomPeerError{"endpoint transport_security does not match its URL"}
	}
	return hostedEndpoint{url: &url, transportSecurity: &security}, nil
}

// Authenticated gateway capabilities inherited by its Bots
type GatewayRoomCatalog struct {
	InstallationID    string
	ProtocolVersions  []*big.Int
	LinkModes         []string
	PersistentProcess bool
	Text              bool
	Attachments       bool
	ExecutionPolicy   *RoomExecutionPolicy
	CatalogDigest     string
	EndpointURL       *string
	EndpointReason    *string
	TransportSecurity *string
}

// Catalog validated from its peer mapping with its digest checked
func GatewayRoomCatalogFromMapping(value any) (*GatewayRoomCatalog, error) {
	fields, err := hostedPeerExactFields(value, "capability catalog", hostedCatalogFields, map[string]bool{"endpoint": true})
	if err != nil {
		return nil, err
	}
	installationID, err := hostedIdentifier(fields["installation_id"], "installation_id")
	if err != nil {
		return nil, err
	}
	rawVersions, err := hostedNonEmptyList(fields["protocol_versions"], "protocol_versions")
	if err != nil {
		return nil, err
	}
	versions, err := hostedProtocolVersions(rawVersions)
	if err != nil {
		return nil, err
	}
	links, err := hostedParseLinkModes(fields["link_modes"])
	if err != nil {
		return nil, err
	}
	flags := map[string]bool{}
	for _, field := range []string{"persistent_process", "text", "attachments"} {
		flag, ok := fields[field].(bool)
		if !ok {
			return nil, &HostedRoomPeerError{field + " must be a boolean"}
		}
		flags[field] = flag
	}
	policy, err := RoomExecutionPolicyFromMapping(fields["execution_policy"])
	if err != nil {
		return nil, err
	}
	var endpoint hostedEndpoint
	if raw, present := fields["endpoint"]; present {
		if endpoint, err = hostedParseEndpoint(raw); err != nil {
			return nil, err
		}
	}
	digest, err := hostedDigest(fields["catalog_digest"], "catalog_digest")
	if err != nil {
		return nil, err
	}
	catalog := &GatewayRoomCatalog{InstallationID: installationID, ProtocolVersions: versions, LinkModes: links,
		PersistentProcess: flags["persistent_process"], Text: flags["text"], Attachments: flags["attachments"],
		ExecutionPolicy: policy, CatalogDigest: digest, EndpointURL: endpoint.url, EndpointReason: endpoint.reason,
		TransportSecurity: endpoint.transportSecurity}
	recomputed, err := hostedCatalogDigest(catalog.AsMapping())
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(recomputed), []byte(catalog.CatalogDigest)) != 1 {
		return nil, &HostedRoomPeerError{"catalog_digest does not match the catalog"}
	}
	return catalog, nil
}

// Canonical catalog mapping, the endpoint present only when advertised
func (c *GatewayRoomCatalog) AsMapping() *pyjson.Object {
	versions := make([]any, len(c.ProtocolVersions))
	for i, v := range c.ProtocolVersions {
		versions[i] = pyjson.ParseInt(v.String())
	}
	links := make([]any, len(c.LinkModes))
	for i, mode := range c.LinkModes {
		links[i] = mode
	}
	value := pyjson.ObjectOf("installation_id", c.InstallationID, "protocol_versions", versions, "link_modes", links,
		"persistent_process", c.PersistentProcess, "text", c.Text, "attachments", c.Attachments,
		"execution_policy", c.ExecutionPolicy.AsMapping(), "catalog_digest", c.CatalogDigest)
	if c.EndpointURL != nil || c.EndpointReason != nil {
		value.Set("endpoint", c.EndpointMapping())
	}
	return value
}

// Normalized self advertised endpoint capability
func (c *GatewayRoomCatalog) EndpointMapping() *pyjson.Object {
	if c.EndpointURL == nil {
		reason := "not_configured"
		if c.EndpointReason != nil && *c.EndpointReason != "" {
			reason = *c.EndpointReason
		}
		return pyjson.ObjectOf("available", false, "reason", reason)
	}
	var security any
	if c.TransportSecurity != nil {
		security = *c.TransportSecurity
	}
	return pyjson.ObjectOf("available", true, "url", *c.EndpointURL, "transport_security", security)
}

// Arguments of one catalog build, nil fields taking the Python defaults
type CatalogOptions struct {
	InstallationID    any
	ProtocolVersions  []any
	LinkModes         []string
	PersistentProcess bool
	Text              *bool
	Attachments       bool
	Endpoint          any
	TargetProfile     any
	ExecutionPolicy   any
}

// Canonical catalog mapping with its digest for the served target profile
func CatalogMapping(ctx context.Context, o CatalogOptions) (*pyjson.Object, error) {
	persistent := o.PersistentProcess && os.Getenv("ICEBATH_DESKTOP") != "1"
	profile, err := hostedIdentifier(o.TargetProfile, "target_profile")
	if err != nil {
		return nil, err
	}
	rawPolicy := o.ExecutionPolicy
	if !pyrepr.Truthy(rawPolicy) {
		if rawPolicy, err = ExecutionPolicyMapping(ctx, profile, nil); err != nil {
			return nil, err
		}
	}
	policy, err := RoomExecutionPolicyFromMapping(rawPolicy)
	if err != nil {
		return nil, err
	}
	if policy.TargetProfile != profile {
		return nil, &HostedRoomPeerError{"execution_policy target_profile does not match the catalog target_profile"}
	}
	if policy.ApprovalMode == "off" {
		return nil, &HostedRoomPeerError{"remote room execution requires manual or smart approvals"}
	}
	installationID, err := hostedIdentifier(o.InstallationID, "installation_id")
	if err != nil {
		return nil, err
	}
	rawVersions := o.ProtocolVersions
	if rawVersions == nil {
		rawVersions = []any{int64(HostedProtocolVersion)}
	}
	versions, err := hostedProtocolVersions(rawVersions)
	if err != nil {
		return nil, err
	}
	versionList := make([]any, len(versions))
	for i, v := range versions {
		versionList[i] = pyjson.ParseInt(v.String())
	}
	linkModes := o.LinkModes
	if linkModes == nil {
		linkModes = []string{"direct", "pull"}
	}
	links := []any{}
	seen := map[string]bool{}
	for _, mode := range linkModes {
		if !seen[mode] {
			seen[mode] = true
			if mode == "direct" {
				links = append(links, mode)
			}
		}
	}
	text := true
	if o.Text != nil {
		text = *o.Text
	}
	endpoint := o.Endpoint
	if endpoint == nil {
		if endpoint, err = localRoomLinkEndpoint(ctx, nil); err != nil {
			return nil, err
		}
	}
	endpointFields, ok := hostedMapping(endpoint)
	if !ok {
		return nil, &PyError{Kind: "TypeError", Msg: "'" + pyTypeName(endpoint) + "' object is not iterable"}
	}
	endpointCopy := pyjson.NewObject()
	if ordered, isObject := endpoint.(*pyjson.Object); isObject {
		for _, key := range ordered.Keys {
			endpointCopy.Set(key, endpointFields[key])
		}
	} else {
		keys := make([]string, 0, len(endpointFields))
		for key := range endpointFields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			endpointCopy.Set(key, endpointFields[key])
		}
	}
	value := pyjson.ObjectOf("installation_id", installationID, "protocol_versions", versionList, "link_modes", links,
		"persistent_process", persistent, "text", text, "attachments", o.Attachments,
		"execution_policy", policy.AsMapping(), "endpoint", endpointCopy)
	digest, err := hostedCatalogDigest(value)
	if err != nil {
		return nil, err
	}
	value.Set("catalog_digest", digest)
	if _, err := GatewayRoomCatalogFromMapping(value); err != nil {
		return nil, err
	}
	return value, nil
}

// Validated endpoint this gateway explicitly advertises, value nil reading the configured one
func localRoomLinkEndpoint(ctx context.Context, value any) (*pyjson.Object, error) {
	configured := value
	if value == nil {
		url, set, err := configuredRoomLinkURL(ctx)
		if err != nil {
			return nil, err
		}
		if set {
			configured = url
		}
	}
	text := ""
	if pyrepr.Truthy(configured) {
		text = pyrepr.Str(configured)
	}
	if pystr.Strip(text) == "" {
		return pyjson.ObjectOf("available", false, "reason", "not_configured"), nil
	}
	url, security, err := ValidateRoomLinkURL(configured)
	if err != nil {
		if _, isPeer := err.(*HostedRoomPeerError); isPeer {
			return pyjson.ObjectOf("available", false, "reason", "invalid_configuration"), nil
		}
		return nil, err
	}
	return pyjson.ObjectOf("available", true, "url", url, "transport_security", security), nil
}

// Least recently used memo of room_link_url per home, sixteen entries like the lru_cache it mirrors
type roomLinkURLMemo struct {
	mu      sync.Mutex
	order   []string
	entries map[string]*string
}

var roomLinkURLCache = &roomLinkURLMemo{entries: map[string]*string{}}

// Restart scoped room_link_url of the config under homeDir, read once per home
func roomLinkURLFromConfig(ctx context.Context, homeDir string) (*string, error) {
	memo := roomLinkURLCache
	memo.mu.Lock()
	if value, hit := memo.entries[homeDir]; hit {
		memo.touch(homeDir)
		memo.mu.Unlock()
		return value, nil
	}
	memo.mu.Unlock()
	scoped := ctx
	if home.Dir(ctx) != homeDir {
		scoped = home.WithOverride(ctx, homeDir)
	}
	cfg, err := LoadGatewayConfig(scoped)
	if err != nil {
		return nil, err
	}
	var value *string
	if cfg.RoomLinkURL != nil {
		if stripped := pystr.Strip(*cfg.RoomLinkURL); stripped != "" {
			value = &stripped
		}
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if _, hit := memo.entries[homeDir]; !hit {
		memo.entries[homeDir] = value
		memo.order = append(memo.order, homeDir)
		if len(memo.order) > 16 {
			delete(memo.entries, memo.order[0])
			memo.order = memo.order[1:]
		}
	}
	return value, nil
}

// Moves key to the most recently used end
func (m *roomLinkURLMemo) touch(key string) {
	for i, existing := range m.order {
		if existing == key {
			m.order = append(append(m.order[:i:i], m.order[i+1:]...), key)
			return
		}
	}
}

// Explicit endpoint from the env override, the profile config, then the root config
func configuredRoomLinkURL(ctx context.Context) (string, bool, error) {
	if override, set := os.LookupEnv("ICEBATH_ROOM_LINK_URL"); set {
		return override, true, nil
	}
	homeDir := home.Dir(ctx)
	configured, err := roomLinkURLFromConfig(ctx, homeDir)
	if err != nil {
		return "", false, err
	}
	if configured != nil {
		return *configured, true, nil
	}
	root := home.Root()
	if root == homeDir {
		return "", false, nil
	}
	if configured, err = roomLinkURLFromConfig(ctx, root); err != nil || configured == nil {
		return "", false, err
	}
	return *configured, true, nil
}

// RoomLink endpoint and its transport, plaintext HTTP accepted only toward loopback
func ValidateRoomLinkURL(value any) (string, string, error) {
	raw := ""
	if pyrepr.Truthy(value) {
		raw = pyrepr.Str(value)
	}
	raw = strings.TrimRight(pystr.Strip(raw), "/")
	invalid := &HostedRoomPeerError{"target_url is invalid"}
	parsed, err := pyurl.Split(raw)
	if err != nil {
		return "", "", invalid
	}
	hostname := pystr.Lower(strings.TrimRight(parsed.Hostname(), "."))
	if _, _, err := parsed.Port(); err != nil {
		return "", "", invalid
	}
	if hostname == "" || strings.Contains(parsed.Netloc, "@") {
		return "", "", invalid
	}
	if parsed.Query != "" || parsed.Fragment != "" {
		return "", "", &HostedRoomPeerError{"target_url must not include query or fragment"}
	}
	scheme := pystr.Lower(parsed.Scheme)
	if scheme == "https" {
		return raw, "tls", nil
	}
	if scheme != "http" {
		return "", "", &HostedRoomPeerError{"target_url must use https"}
	}
	if hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") {
		return raw, "loopback", nil
	}
	if addr, ok := pyipaddress.IPAddress(hostname); ok && pyipaddress.IsLoopback(addr) {
		return raw, "loopback", nil
	}
	return "", "", &HostedRoomPeerError{"target_url must use https outside the local machine"}
}

// The one truthful catalog advertised by this local process, a persistent process unless Desktop managed
func LocalCatalogMapping(ctx context.Context, o CatalogOptions) (*pyjson.Object, error) {
	o.PersistentProcess = true
	return CatalogMapping(ctx, o)
}
