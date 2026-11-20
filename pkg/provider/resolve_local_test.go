package provider

import "testing"

func TestIsRateLimitedAuthErrorExportMatchesTheResolverPredicate(t *testing.T) {
	for _, err := range []error{&AuthError{Message: "rate limited", Code: "rate_limited", Provider: "openai"}, &AuthError{Message: "no key", Code: "no_provider_configured"}} {
		if IsRateLimitedAuthError(err) != isRateLimitedAuthError(err) {
			t.Fatalf("exported wrapper disagrees for %v", err)
		}
	}
	if IsRateLimitedAuthError(nil) {
		t.Fatal("nil is never rate limited")
	}
}

func TestFormatAuthErrorExportMatchesTheFormatter(t *testing.T) {
	err := &AuthError{Message: "key rejected", Code: "auth", Provider: "openai", ReloginRequired: true}
	if FormatAuthError(err) != formatAuthError(err) || FormatAuthError(err) == "" {
		t.Fatalf("got %q", FormatAuthError(err))
	}
}

func TestPrimaryFailureWordingExportMatchesTheWording(t *testing.T) {
	err := &AuthError{Message: "quota exceeded", Code: "rate_limited", Provider: "openai"}
	gotLog, gotUser := PrimaryFailureWording(err)
	wantLog, wantUser := primaryFailureWording(err)
	if gotLog != wantLog || gotUser != wantUser || gotLog == "" {
		t.Fatalf("got %q %q", gotLog, gotUser)
	}
}
