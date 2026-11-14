package contractgen

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

// A contract enum that drifts from its domain twin stops generation with both names and the odd values
func TestEnumParityNamesTheDrift(t *testing.T) {
	if err := checkEnumParity(); err != nil {
		t.Fatalf("catalog enums drifted: %v", err)
	}
	drift := enumPair{"ConnectorToolFacet", pydantic.Enum("ConnectorToolFacet", "read", "read", "write", "write"), "ConnectorTool.facet", []string{"read", "write", "destructive"}}
	if err := parityOf(drift); err == nil || err.Error() != "enum parity failed: ConnectorToolFacet vs ConnectorTool.facet: ['destructive']" {
		t.Fatalf("parity error = %v", err)
	}
}

// The rendered TypeScript names every method, canonical method, server request and event table
func TestRenderedTypeScriptSections(t *testing.T) {
	files, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	ts := files[TSPath]
	for _, want := range []string{header, "export const RPC_METHODS = [", "export const CANONICAL_RPC_METHODS = [",
		"export const SERVER_REQUEST_METHODS = [", "export const GATEWAY_EVENT_TYPES = [", "export type ConnectorErrorReason = "} {
		if !strings.Contains(ts, want) {
			t.Errorf("TypeScript misses %q", want)
		}
	}
	if !strings.HasSuffix(ts, "\n") || strings.HasSuffix(ts, "\n\n") || strings.Contains(ts, " \n") {
		t.Error("TypeScript is not tidy")
	}
}
