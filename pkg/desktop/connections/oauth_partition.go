package connections

import (
	"strings"
)

// LegacyOauthPartition is the shared cookie jar of the primary, the v1 remote, cloud and portal flows
const LegacyOauthPartition = "persist:hermes-remote-oauth"

const connectionPartitionPrefix = LegacyOauthPartition + "-conn-"

// ResolveOauthPartitionOptions is the registry snapshot and the pending login identity a partition is chosen from
type ResolveOauthPartitionOptions struct {
	Registry        *ConnectionRegistry
	V1RemoteURL     string
	ConnectionID    string
	PendingAuthMode string
	PendingKind     string
}

// Scheme, host and path without query, fragment or trailing slash of an http URL, empty otherwise
func normalizeForMatch(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	parsed, err := parseURL(value)
	if err != nil || parsed.Protocol() != "http:" && parsed.Protocol() != "https:" {
		return ""
	}
	return parsed.Protocol() + "//" + parsed.Host() + trailingSlashes.ReplaceAllString(parsed.Pathname(), "")
}

func matchesBase(requestNorm, baseNorm string) bool {
	return requestNorm == baseNorm || strings.HasPrefix(requestNorm, baseNorm+"/")
}

func sanitizePartitionComponent(id string) string {
	return strings.ReplaceAll(encodeURIComponent(id), "%", "_")
}

// ResolveOauthPartition is the session partition a cookie authenticated request against requestURL rides
func ResolveOauthPartition(requestURL string, o ResolveOauthPartitionOptions) string {
	requestNorm := normalizeForMatch(requestURL)
	if requestNorm == "" || o.Registry == nil || o.Registry.Connections == nil {
		return LegacyOauthPartition
	}
	primaryID := o.Registry.Primary
	v1Norm := normalizeForMatch(o.V1RemoteURL)
	if wantedID := strings.TrimSpace(o.ConnectionID); wantedID != "" {
		own := o.Registry.Find(wantedID)
		if own == nil {
			if o.PendingKind == "remote" && o.PendingAuthMode == "oauth" && !(v1Norm != "" && requestNorm == v1Norm) {
				return connectionPartitionPrefix + sanitizePartitionComponent(wantedID)
			}
			return LegacyOauthPartition
		}
		ownBase := normalizeForMatch(own.URL)
		if wantedID != primaryID && own.Kind == "remote" && own.AuthMode == "oauth" && ownBase != "" && !(v1Norm != "" && ownBase == v1Norm) {
			return connectionPartitionPrefix + sanitizePartitionComponent(wantedID)
		}
		return LegacyOauthPartition
	}
	bestBase, bestID := "", ""
	for _, entry := range o.Registry.Connections {
		id := strings.TrimSpace(entry.ID)
		if id == "" || id == primaryID || entry.Kind != "remote" || entry.AuthMode != "oauth" {
			continue
		}
		base := normalizeForMatch(entry.URL)
		if base == "" || v1Norm != "" && base == v1Norm || !matchesBase(requestNorm, base) {
			continue
		}
		if bestID == "" || len(base) > len(bestBase) || len(base) == len(bestBase) && id < bestID {
			bestBase, bestID = base, id
		}
	}
	if bestID == "" {
		return LegacyOauthPartition
	}
	return connectionPartitionPrefix + sanitizePartitionComponent(bestID)
}
