package observability

import "testing"

func TestCommittedFallbackRowValidatesAgainstThePackageSchema(t *testing.T) {
	s := openStore(t, t.TempDir())
	fields := CompressionFields("auto", "success", 80_000, 100_000, "summary_generation_failed")
	mustRecord(t, s, "icebath.compression.count", fields, testResource())
	paths := mustExport(t, s)
	if len(paths) != 1 {
		t.Fatalf("packages %v", paths)
	}
	pkg := readPackage(t, paths[0])
	validatePackage(t, 4, pkg)
	var rows []map[string]any
	for _, m := range pkg["metrics"].([]any) {
		if row := m.(map[string]any); row["name"] == "icebath.compression.count" {
			rows = append(rows, row)
		}
	}
	if len(rows) != 1 || rows[0]["dimensions"].(map[string]any)["failure_class"] != "summary_generation_failed" {
		t.Fatalf("rows %v", rows)
	}
}
