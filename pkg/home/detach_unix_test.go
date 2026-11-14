//go:build !windows

package home

import "testing"

func TestDetachAttrStartsANewSession(t *testing.T) {
	if attr := DetachAttr(true); attr == nil || !attr.Setsid {
		t.Fatalf("%+v", attr)
	}
}
