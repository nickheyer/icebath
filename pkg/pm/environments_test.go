package pm

import (
	"reflect"
	"testing"
)

func TestExportedImportPathEntries(t *testing.T) {
	importPath.Lock()
	saved := importPath.entries
	importPath.entries = []string{"root", "site"}
	importPath.Unlock()
	defer func() { importPath.Lock(); importPath.entries = saved; importPath.Unlock() }()
	if got := ImportPathEntries(); !reflect.DeepEqual(got, []string{"root", "site"}) {
		t.Fatalf("import path %v", got)
	}
}
