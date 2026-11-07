package provider

import "testing"

func TestResolveNousRuntimeCredentialsIsTheRuntimeResolver(t *testing.T) {
	env := anonPresentEnv(t)
	got, gotErr := ResolveNousRuntimeCredentials(env, false, "")
	want, wantErr := resolveNousRuntimeCredentials(env, false, "")
	if (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) || len(got) != len(want) {
		t.Fatalf("export = %v %v, original = %v %v", got, gotErr, want, wantErr)
	}
}
