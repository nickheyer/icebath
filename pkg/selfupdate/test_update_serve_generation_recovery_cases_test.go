package selfupdate

import (
	"errors"
	"reflect"
	"regexp"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Payload text json.dumps writes for a recovery request
func servePayloadText(t *testing.T, payload any) string {
	t.Helper()
	text, err := pyjson.Dumps(payload)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// Fails unless err is a ValueError whose text matches pattern
func requireRecoveryValueError(t *testing.T, err error, pattern string) {
	t.Helper()
	var py *pm.PyError
	if !errors.As(err, &py) || py.Class != "ValueError" {
		t.Fatalf("want ValueError, got %T %v", err, err)
	}
	if !regexp.MustCompile(pattern).MatchString(py.Message) {
		t.Fatalf("ValueError %q does not match %q", py.Message, pattern)
	}
}

func TestSkipUnitNamesAreFilteredToTheServeFamily(t *testing.T) {
	payload := pyjson.ObjectOf("profiles", []any{}, "serve_units", pyjson.ObjectOf(
		"recover", true,
		"skip", []any{
			"icebath-serve",
			"icebath-serve-work.service",
			pyjson.ObjectOf("scope", "system", "unit", "icebath-serve"),
			pyjson.ObjectOf("scope", "user", "unit", "icebath-serve-work.service"),
			"icebath-server",
			"../../etc/systemd/evil",
			"icebath-serve; rm -rf /",
			"icebath-gateway",
		}))
	_, _, recoverServe, skip, err := parseRecoveryPayload(servePayloadText(t, payload))
	if err != nil {
		t.Fatal(err)
	}
	if !recoverServe {
		t.Fatal("recover_serve is not True")
	}
	want := []any{"icebath-serve", "icebath-serve-work", "system/icebath-serve", "user/icebath-serve-work"}
	if !reflect.DeepEqual(skip, want) {
		t.Fatalf("skip = %#v, want %#v", skip, want)
	}
}

func TestMalformedServeBlockIsRejected(t *testing.T) {
	_, _, _, _, err := parseRecoveryPayload(servePayloadText(t, pyjson.ObjectOf("profiles", []any{}, "serve_units", []any{"nope"})))
	requireRecoveryValueError(t, err, "serve_units")
}

func TestNonStringSkipEntriesAreRejected(t *testing.T) {
	payload := pyjson.ObjectOf("profiles", []any{}, "serve_units", pyjson.ObjectOf("recover", true, "skip", []any{7}))
	_, _, _, _, err := parseRecoveryPayload(servePayloadText(t, payload))
	requireRecoveryValueError(t, err, "skip list")
}

func TestAbsentServeBlockDefaultsToNoServeRecovery(t *testing.T) {
	_, _, recoverServe, skip, err := parseRecoveryPayload(servePayloadText(t, pyjson.ObjectOf("profiles", []any{})))
	if err != nil {
		t.Fatal(err)
	}
	if recoverServe {
		t.Fatal("recover_serve is not False")
	}
	if len(skip) != 0 {
		t.Fatalf("skip = %#v, want []", skip)
	}
}
