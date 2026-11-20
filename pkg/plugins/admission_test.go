package plugins

import (
	"errors"
	"testing"
)

func TestDependencyConflictNamesThePluginAndIsAnAdmissionRefusal(t *testing.T) {
	err := error(&DependencyConflict{Cause: "no solution for foo==2", Plugin: "fixture"})
	want := "Plugin 'fixture' conflicts with the dependencies pinned by Icebath core or an enabled plugin, so it was not admitted. Resolver: no solution for foo==2"
	var refused *AdmissionRefused
	var conflict *DependencyConflict
	if err.Error() != want || !errors.As(err, &refused) || refused.Error() != want || !errors.As(err, &conflict) || conflict.Cause != "no solution for foo==2" {
		t.Fatalf("%v", err)
	}
	if got := (&DependencyConflict{Cause: "x"}).Error(); got != "The plugin selection conflicts with the dependencies pinned by Icebath core or an enabled plugin, so it was not admitted. Resolver: x" {
		t.Fatal(got)
	}
	if errors.As(error(&AdmissionRefused{Msg: "m"}), &conflict) {
		t.Fatal("a plain refusal is not a conflict")
	}
}
