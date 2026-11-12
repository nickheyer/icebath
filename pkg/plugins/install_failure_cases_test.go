package plugins_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Third party error whose every method a classifier could run panics
type hostileError struct{ text string }

func (e *hostileError) Error() string            { return e.text }
func (e *hostileError) Unwrap() error            { panic("hostile getattr") }
func (e *hostileError) FailureClass() string     { panic("hostile getattr") }
func (e *hostileError) Is(error) bool            { panic("hostile getattr") }
func (e *hostileError) As(any) bool              { panic("hostile getattr") }
func (e *hostileError) PyExceptionMRO() []string { panic("hostile getattr") }

func TestFailureClassifiersNeverRaiseOnAnExceptionWhoseAttributeHooksRaise(t *testing.T) {
	saved := observability.SkillSourceIDAliasesHook
	t.Cleanup(func() { observability.SkillSourceIDAliasesHook = saved })
	observability.SkillSourceIDAliasesHook = skills.SourceIDAliases
	if got := plugins.PublishFailureClass(&hostileError{"x"}); got != "deps_failed" {
		t.Fatalf("publish class %q", got)
	}
	if got := mcp.ProbeFailureClass(&hostileError{"x"}); got != "connect_failed" {
		t.Fatalf("probe class %q", got)
	}
	for _, kind := range slices.Sorted(maps.Keys(observability.ExtensionKinds)) {
		dims := observability.ExtensionInstallFields(kind, "url", nil, "failed", nil, nil, &hostileError{"x"})
		if dims["failure_class"] != "exception" {
			t.Fatalf("%s class %q", kind, dims["failure_class"])
		}
		row := map[string]any{}
		for k, v := range dims {
			row[k] = v
		}
		if !observability.CounterDimensionsAreValid(observability.ExtensionInstallMetric, row) {
			t.Fatalf("%s row %v", kind, row)
		}
	}
}
