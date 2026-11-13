package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// PortalToolsTTLSeconds keeps a member's cached tool listing fresh for a day
const PortalToolsTTLSeconds = 24 * 60 * 60

// PortalToolsClient is the Portal surface a tools read calls
type PortalToolsClient interface {
	Origin() (string, error)
	AuthorizationToken() (string, error)
	Tools(slug, ifNoneMatch string) (any, error)
}

// Source values a ToolsRead may carry, the Literal of its source field
var ToolsReadSources = []string{"cache", "network", "revalidated"}

// ToolsRead is one connector tool listing with where it came from and whether it is stale
type ToolsRead struct {
	Connector      string
	ToolkitVersion string
	Etag           string
	FetchedAt      float64
	Source         string
	Stale          bool
	Tools          []ConnectorTool
}

// Short hash of the token subject keying a member's cache, empty when the token is no readable JWT
func portalMemberKey(token string) string {
	parts := strings.Split(token, ".")
	if token == "" || len(parts) != 3 {
		return ""
	}
	payload := parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4)
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	decoded, err := pyjson.Loads(string(raw))
	if err != nil {
		return ""
	}
	claims, isDict := decoded.(*pyjson.Object)
	if !isDict {
		return ""
	}
	subject, _ := claims.GetOr("sub", nil).(string)
	if subject == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:])[:16]
}

// Cache file of one connector listing under the profile home, keyed by origin and member
func portalToolsCachePath(env *Env, origin, memberKey, slug string) string {
	sum := sha256.Sum256([]byte(origin))
	return filepath.Join(envHome(env), "cache", "connectors", hex.EncodeToString(sum[:])[:16], memberKey, slug+".json")
}

// Python float() of a decoded JSON value, false where float() raises
func portalFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case *big.Int:
		f, _ := new(big.Float).SetInt(t).Float64()
		return f, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(t, "_", "")), 64)
		return f, err == nil
	}
	return 0, false
}

// Items a for loop yields over a decoded JSON value, a dict its keys and a string its characters, false when not iterable
func portalIterated(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case *pyjson.Object:
		keys := make([]any, len(t.Keys))
		for i, k := range t.Keys {
			keys[i] = k
		}
		return keys, true
	case string:
		chars := []any{}
		for _, r := range t {
			chars = append(chars, string(r))
		}
		return chars, true
	}
	return nil, false
}

// Cached listing at path, nil when missing, malformed or holding an invalid tool
func portalCachedTools(path string) *ToolsRead {
	raw := pyjson.NewObject()
	if data, err := os.ReadFile(path); err == nil {
		if parsed, err := pyjson.Loads(strings.TrimPrefix(string(data), "\ufeff")); err == nil {
			if obj, isDict := parsed.(*pyjson.Object); isDict {
				raw = obj
			}
		}
	}
	rows, iterable := portalIterated(raw.GetOr("tools", nil))
	if !iterable {
		return nil
	}
	tools := make([]ConnectorTool, 0, len(rows))
	for _, row := range rows {
		obj, isDict := row.(*pyjson.Object)
		if !isDict {
			return nil
		}
		tool, err := wireConnectorTool(obj)
		if err != nil {
			return nil
		}
		tools = append(tools, tool)
	}
	fields := map[string]any{}
	for _, key := range []string{"connector", "toolkit_version", "etag", "fetched_at"} {
		value, present := raw.Get(key)
		if !present {
			return nil
		}
		fields[key] = value
	}
	fetchedAt, isFloat := portalFloat(fields["fetched_at"])
	if !isFloat {
		return nil
	}
	return &ToolsRead{Connector: pyrepr.Str(fields["connector"]), ToolkitVersion: pyrepr.Str(fields["toolkit_version"]),
		Etag: pyrepr.Str(fields["etag"]), FetchedAt: fetchedAt, Source: "cache", Stale: false, Tools: tools}
}

// Alias keyed dump of one tool as the cache stores it
func portalToolDump(tool ConnectorTool) *pyjson.Object {
	return pyjson.ObjectOf("slug", tool.Slug, "name", tool.Name, "description", tool.Description, "facet", tool.Facet,
		"hints", portalStrings(tool.Hints), "categories", portalStrings(tool.Categories), "noAuth", tool.NoAuth, "deprecated", tool.Deprecated)
}

func portalStrings(items []string) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

// Writes the listing to its cache file and answers it
func portalStoreTools(ctx context.Context, path string, entry ToolsRead) (ToolsRead, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return entry, err
	}
	tools := make([]any, len(entry.Tools))
	for i, tool := range entry.Tools {
		tools[i] = portalToolDump(tool)
	}
	err := config.AtomicJSONWrite(ctx, path, pyjson.ObjectOf("etag", entry.Etag, "toolkit_version", entry.ToolkitVersion,
		"connector", entry.Connector, "tools", tools, "fetched_at", entry.FetchedAt))
	return entry, err
}

// Error of a listing the Portal could not serve
func portalToolsUnavailable() error {
	return &ToolGatewayError{Message: "portal tools unavailable", Code: "INVALID_RESPONSE", Kind: PortalKindToolsUnavailable}
}

// Seconds since the epoch as time.time answers
func portalNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Tool listing of one connector from the member's day long cache, revalidated or fetched when stale or refreshed
func ReadTools(ctx context.Context, env *Env, slug string, client PortalToolsClient, refresh bool, now func() float64) (ToolsRead, error) {
	if now == nil {
		now = portalNow
	}
	if err := ValidateSlug(slug); err != nil {
		return ToolsRead{}, err
	}
	token, err := client.AuthorizationToken()
	if err != nil {
		return ToolsRead{}, err
	}
	memberKey := portalMemberKey(token)
	if memberKey == "" {
		listing, err := client.Tools(slug, "")
		if err != nil {
			return ToolsRead{}, err
		}
		fresh, isListing := listing.(ConnectorToolsListing)
		if !isListing {
			return ToolsRead{}, portalToolsUnavailable()
		}
		return ToolsRead{Connector: fresh.Connector, ToolkitVersion: fresh.ToolkitVersion, Etag: fresh.Etag, FetchedAt: now(),
			Source: "network", Tools: fresh.Tools}, nil
	}
	origin, err := client.Origin()
	if err != nil {
		return ToolsRead{}, err
	}
	path := portalToolsCachePath(env, origin, memberKey, slug)
	cached := portalCachedTools(path)
	fetchedAt := now()
	if cached != nil && !refresh {
		if age := fetchedAt - cached.FetchedAt; age >= 0 && age < PortalToolsTTLSeconds {
			return *cached, nil
		}
	}
	etag := ""
	if cached != nil {
		etag = cached.Etag
	}
	listing, err := client.Tools(slug, etag)
	if err != nil {
		var gatewayErr *ToolGatewayError
		if !errors.As(err, &gatewayErr) {
			return ToolsRead{}, err
		}
		switch gatewayErr.Kind {
		case gatewayKindUnavailable:
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return ToolsRead{}, removeErr
			}
			return ToolsRead{}, err
		case gatewayKindAuth:
			return ToolsRead{}, err
		}
		if cached == nil {
			return ToolsRead{}, err
		}
		stale := *cached
		stale.Stale = true
		return stale, nil
	}
	if _, notModified := listing.(PortalNotModified); notModified {
		if cached == nil {
			return ToolsRead{}, portalToolsUnavailable()
		}
		revalidated := *cached
		revalidated.FetchedAt, revalidated.Source = fetchedAt, "revalidated"
		return portalStoreTools(ctx, path, revalidated)
	}
	fresh := listing.(ConnectorToolsListing)
	return portalStoreTools(ctx, path, ToolsRead{Connector: fresh.Connector, ToolkitVersion: fresh.ToolkitVersion, Etag: fresh.Etag,
		FetchedAt: fetchedAt, Source: "network", Tools: fresh.Tools})
}
