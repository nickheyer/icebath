package files

import (
	"testing"
)

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestSelectPathsDialogPropertiesLetsADirectoryPickerCreateANewFolderMacOSHidesNewFolderWithoutCreateDirectory(t *testing.T) {
	p := SelectPathsDialogProperties(true, false)
	if !contains(p, "openDirectory") || !contains(p, "createDirectory") {
		t.Fatal(p)
	}
}

func TestSelectPathsDialogPropertiesKeepsFilePickersFileOnlyAndMultiSelectByDefault(t *testing.T) {
	p := SelectPathsDialogProperties(false, nil)
	if !contains(p, "openFile") || !contains(p, "multiSelections") || contains(p, "openDirectory") || contains(p, "createDirectory") {
		t.Fatal(p)
	}
}

func TestSelectPathsDialogPropertiesDropsMultiSelectOnlyWhenTheCallerOptsOut(t *testing.T) {
	if !contains(SelectPathsDialogProperties(true, nil), "multiSelections") || contains(SelectPathsDialogProperties(true, false), "multiSelections") {
		t.Fatal("multi select")
	}
}
