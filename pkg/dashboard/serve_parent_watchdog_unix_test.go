//go:build !windows

package dashboard

import (
	"errors"
	"testing"
)

func TestPsMarkerProbeClassifiesMissingProcessVsOtherPsFailures(t *testing.T) {
	var gone *ProcessLookupError
	if _, err := psStartMarker(4242, 2, "", "ps: 4242: No such process"); !errors.As(err, &gone) {
		t.Fatalf("missing process must be ProcessLookupError, got %v", err)
	}
	_, err := psStartMarker(4242, 2, "", "ps: temporary process table failure")
	if err == nil || errors.As(err, &gone) {
		t.Fatalf("other ps failures must stay a plain error, got %v", err)
	}
}
