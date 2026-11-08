package provider

import (
	"testing"
	"time"
)

func TestResolveNousRuntimeCredentialsIsTheRuntimeResolver(t *testing.T) {
	env := anonPresentEnv(t)
	got, gotErr := ResolveNousRuntimeCredentials(env, false, "")
	want, wantErr := resolveNousRuntimeCredentials(env, false, "")
	if (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) || len(got) != len(want) {
		t.Fatalf("export = %v %v, original = %v %v", got, gotErr, want, wantErr)
	}
}

func TestResolveNousRuntimeCredentialsWithIsTheOptionResolver(t *testing.T) {
	env := anonPresentEnv(t)
	opts := NousResolveOptions{Timeout: 5 * time.Second}
	got, gotErr := ResolveNousRuntimeCredentialsWith(env, opts)
	want, wantErr := resolveNousRuntimeCredentialsWith(env, opts)
	if (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) || len(got) != len(want) {
		t.Fatalf("export = %v %v, original = %v %v", got, gotErr, want, wantErr)
	}
}
