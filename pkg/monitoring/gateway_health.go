package monitoring

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// GatewayMetric is one exported gauge with its value and string attributes
type GatewayMetric struct {
	Name       string
	Value      any
	Attributes map[string]string
}

// Predicate true when the text holds any of the needles
func containsAny(needles ...string) func(string) bool {
	return func(text string) bool {
		for _, n := range needles {
			if strings.Contains(text, n) {
				return true
			}
		}
		return false
	}
}

// Stable opaque sha256 key of a source id that never exports the id itself
func safeInstanceID(raw any) string {
	if !pyrepr.Truthy(raw) {
		raw = "unknown"
	}
	sum := sha256.Sum256(pystr.EncodeUTF8Replace(pyrepr.Str(raw)))
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}
