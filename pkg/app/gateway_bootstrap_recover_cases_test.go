package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestFailureNamesTheReasonAndItsRemedy(t *testing.T) {
	receipt := gateway.ObservedEndpoint{State: "incompatible", ReasonCode: "runtime_protocol",
		Detail: "the gateway belongs to another Icebath install; stop it there"}
	got := strings.Split(tuiBootstrapFailureMessage(receipt), "\n")
	want := []string{"gateway incompatible: runtime_protocol",
		"gateway incompatible: the gateway belongs to another Icebath install; stop it there"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
}

func TestFailureMessageKeepsARuntimeExitDetailVerbatim(t *testing.T) {
	receipt := gateway.ObservedEndpoint{State: "failed", ReasonCode: "runtime_exited", Detail: "  boom\n  trace"}
	if got := tuiBootstrapFailureMessage(receipt); got != "gateway failed: runtime_exited\n  boom\n  trace" {
		t.Fatalf("%q", got)
	}
}

func TestFailureMessageWithoutDetailIsTheVerdictAlone(t *testing.T) {
	if got := tuiBootstrapFailureMessage(gateway.ObservedEndpoint{State: "absent"}); got != "gateway absent: not ready" {
		t.Fatalf("%q", got)
	}
}

func TestClipboardHelperWithoutDestinationExitsNonZero(t *testing.T) {
	var out, errOut strings.Builder
	if code := RunTUIClipboardImage(t.Context(), nil, &out, &errOut); code != 1 || out.Len() != 0 {
		t.Fatalf("%d %q %q", code, out.String(), errOut.String())
	}
}
