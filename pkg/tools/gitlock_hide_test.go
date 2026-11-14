package tools

import (
	"reflect"
	"testing"
)

func TestWindowsHideAttrMatchesTheUnexportedAttr(t *testing.T) {
	if !reflect.DeepEqual(WindowsHideAttr(), windowsHideAttr()) {
		t.Fatalf("exported %+v unexported %+v", WindowsHideAttr(), windowsHideAttr())
	}
}
