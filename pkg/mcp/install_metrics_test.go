package mcp

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pystr"

	"github.com/nickheyer/icebath/pkg/observability"
)

func TestRecordExtensionInstallArgsPassNoneForEmpty(t *testing.T) {
	nameArg, classArg := extensionInstallArgs("", "")
	if nameArg != nil || classArg != nil {
		t.Fatalf("empty name and class must be None, got %v %v", nameArg, classArg)
	}
	nameArg, classArg = extensionInstallArgs("github", "auth_failed")
	if nameArg != "github" || classArg != "auth_failed" {
		t.Fatalf("set values must pass through, got %v %v", nameArg, classArg)
	}
}

func TestRecordExtensionInstallFieldsForCustomAndCatalogFailures(t *testing.T) {
	nameArg, classArg := extensionInstallArgs("", "")
	err := &CatalogError{Msg: "bad manifest"}
	fields := observability.ExtensionInstallFields("mcp_server", "custom", nameArg, "failed", classArg, nil, err)
	if fields["name"] != observability.Custom || fields["outcome"] != "failed" || fields["failure_class"] != err.FailureClass() {
		t.Fatalf("custom failure fields %v", fields)
	}
	nameArg, classArg = extensionInstallArgs("", "probe_failed")
	fields = observability.ExtensionInstallFields("mcp_server", "custom", nameArg, "failed", classArg, nil, errors.New("x"))
	if fields["failure_class"] == err.FailureClass() {
		t.Fatalf("explicit failure class must win over the error, got %v", fields)
	}
	fields = observability.ExtensionInstallFields("mcp_server", "catalog", "github", "success", nil, nil, nil)
	if fields["failure_class"] != "none" || fields["outcome"] != "success" {
		t.Fatalf("success fields %v", fields)
	}
}

func TestMCPCatalogNamesFeedInstallMetricNames(t *testing.T) {
	observability.ResetCatalogCaches()
	t.Cleanup(observability.ResetCatalogCaches)
	entries, err := fs.ReadDir(embeddedCatalog, "catalog")
	if err != nil || len(entries) == 0 {
		t.Fatalf("embedded catalog %v", err)
	}
	shipped := entries[0].Name()
	if got := observability.ExtensionMetricName("mcp_server", "official/"+shipped); got != pystr.Lower(shipped) {
		t.Fatalf("shipped catalog entry %s reads as %s", shipped, got)
	}
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "GitHub"), 0o755)
	os.Mkdir(filepath.Join(dir, "linear"), 0o755)
	os.WriteFile(filepath.Join(dir, "notes.yaml"), []byte("x"), 0o644)
	t.Setenv(catalogRootOverride, dir)
	observability.ResetCatalogCaches()
	names, err := mcpCatalogNames()
	slices.Sort(names)
	if err != nil || strings.Join(names, ",") != "github,linear" {
		t.Fatalf("names %v %v", names, err)
	}
	if got := observability.ExtensionMetricName("mcp_server", "GitHub"); got != "github" {
		t.Fatalf("override catalog entry reads as %s", got)
	}
	if got := observability.ExtensionMetricName("mcp_server", "my-local-server"); got != observability.Custom {
		t.Fatalf("custom server reads as %s", got)
	}
	t.Setenv(catalogRootOverride, filepath.Join(dir, "absent"))
	observability.ResetCatalogCaches()
	if got := observability.ExtensionMetricName("mcp_server", "github"); got != observability.Custom {
		t.Fatalf("unreadable catalog root must be the empty set, got %s", got)
	}
}
