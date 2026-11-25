package provider

import (
	"errors"
	"testing"
)

type fixed []Profile

func (f fixed) Profiles() []Profile { return f }

func TestRegistryResolvesNamesBeforeAliases(t *testing.T) {
	host := New()
	nested := new(Registry)
	host.Register(nested)
	host.Register(fixed{{Name: "first", Aliases: []string{"shared", "second"}}})
	nested.Register(fixed{{Name: "second", DisplayName: "Second", Aliases: []string{"shared"}}})

	if got := len(host.Profiles()); got != 2 {
		t.Fatalf("Profiles() returned %d profiles, want 2", got)
	}
	if names := []string{host.Profiles()[0].Name, host.Profiles()[1].Name}; names[0] != "second" || names[1] != "first" {
		t.Fatalf("Profiles() order = %v, want registration order [second first]", names)
	}
	if profile, ok := host.Profile("  SECOND "); !ok || profile.Name != "second" {
		t.Fatalf("Profile(\"  SECOND \") = %q, %v, want the profile named second even though first claims it as an alias", profile.Name, ok)
	}
	if profile, ok := host.Profile("shared"); !ok || profile.Name != "second" {
		t.Fatalf("Profile(\"shared\") = %q, %v, want the first profile that claims the alias", profile.Name, ok)
	}
	if _, ok := host.Profile("missing"); ok {
		t.Fatal("Profile(\"missing\") resolved, want not found")
	}
	if label := (Profile{Name: "plain"}).Label(); label != "plain" {
		t.Fatalf("Label() = %q, want the name when there is no display name", label)
	}
	if profile, _ := host.Profile("second"); profile.Label() != "Second" {
		t.Fatalf("Label() = %q, want the display name", profile.Label())
	}
}

func TestAPIError(t *testing.T) {
	kinds := map[int]ErrorKind{401: ErrAuth, 403: ErrAuth, 404: ErrModelNotFound, 413: ErrContextOverflow, 429: ErrRateLimit, 500: ErrServer, 529: ErrServer, 400: ErrInvalidRequest, 422: ErrInvalidRequest, 200: ErrUnknown}
	for status, want := range kinds {
		if got := KindOf(status); got != want {
			t.Errorf("KindOf(%d) = %q, want %q", status, got, want)
		}
	}
	retryable := map[ErrorKind]bool{ErrRateLimit: true, ErrServer: true, ErrNetwork: true, ErrEmptyResponse: true, ErrAuth: false, ErrInvalidRequest: false, ErrContentFilter: false, ErrUnknown: false}
	for kind, want := range retryable {
		if got := (&APIError{Kind: kind}).Retryable(); got != want {
			t.Errorf("Retryable() of %q = %v, want %v", kind, got, want)
		}
	}
	cause := errors.New("boom")
	failure := &APIError{Provider: "p", Model: "m", StatusCode: 429, Message: "slow down", Kind: ErrRateLimit, Err: cause}
	if got, want := failure.Error(), "p/m: slow down (429 rate_limit)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(failure, cause) {
		t.Error("errors.Is did not reach the wrapped cause")
	}
	if got, want := (&APIError{Provider: "p", Model: "m", Message: "reset", Kind: ErrNetwork}).Error(), "p/m: reset (network)"; got != want {
		t.Errorf("Error() without a status = %q, want %q", got, want)
	}
}
