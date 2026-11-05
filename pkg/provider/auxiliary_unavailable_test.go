package provider

import (
	"log/slog"
	"strings"
	"testing"
)

func TestNousCredentialFailureIsRememberedAndWarnedOnce(t *testing.T) {
	ctx := modelsTestHome(t)
	auxResetNousCredentialFailures()
	t.Cleanup(auxResetNousCredentialFailures)
	logs := auxCaptureLogs(t)
	err := &AuthError{Message: "Invalid refresh token", Provider: "nous", Code: "invalid_grant", ReloginRequired: true}
	detail := RecordNousCredentialFailure(ctx, err)
	RecordNousCredentialFailure(ctx, err)
	if !strings.HasPrefix(detail, "Nous Portal runtime credentials unavailable: ") {
		t.Fatalf("detail %q", detail)
	}
	if !strings.Contains(detail, "invalid_grant") || !strings.Contains(detail, "icebath model") {
		t.Fatalf("detail %q", detail)
	}
	if got, ok := NousCredentialFailureDetail(); !ok || got != detail {
		t.Fatalf("remembered %q %v", got, ok)
	}
	if recs := logs.matching(detail); len(recs) != 1 || recs[0].level != slog.LevelWarn {
		t.Fatalf("records %+v", recs)
	}
	ClearNousCredentialFailure()
	if _, ok := NousCredentialFailureDetail(); ok {
		t.Fatal("cleared detail is None")
	}
}

func TestNeverLoggedInIsDebugButADeadCredentialWarns(t *testing.T) {
	ctx := modelsTestHome(t)
	auxResetNousCredentialFailures()
	t.Cleanup(auxResetNousCredentialFailures)
	logs := auxCaptureLogs(t)
	quiet := RecordNousCredentialFailure(ctx, &AuthError{Message: "Icebath is not logged into Nous Portal.", Provider: "nous", ReloginRequired: true})
	loud := RecordNousCredentialFailure(ctx, &AuthError{Message: "Invalid refresh token", Provider: "nous", Code: "invalid_grant", ReloginRequired: true})
	for _, r := range logs.matching(quiet) {
		if r.level != slog.LevelDebug {
			t.Fatalf("never logged in must be debug: %+v", r)
		}
	}
	if recs := logs.matching(loud); len(recs) != 1 || recs[0].level != slog.LevelWarn {
		t.Fatalf("dead credential must warn: %+v", recs)
	}
	if len(logs.matching(quiet)) != 1 || !strings.Contains(quiet, "icebath model") {
		t.Fatalf("quiet %q records %+v", quiet, logs.records)
	}
}

func TestMissingProviderCredentialsMessageComesFromTheRegistry(t *testing.T) {
	ctx := modelsTestHome(t)
	if got := MissingProviderCredentialsMessage(ctx, "llamacpp"); !strings.Contains(got, "no local llama.cpp server is running") {
		t.Fatalf("llamacpp %q", got)
	}
	if got := MissingProviderCredentialsMessage(ctx, "no-such-provider"); !strings.Contains(got, "Set the NO_SUCH_PROVIDER_API_KEY environment variable, or switch") {
		t.Fatalf("unknown %q", got)
	}
}
