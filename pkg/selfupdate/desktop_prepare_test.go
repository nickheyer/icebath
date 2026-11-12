package selfupdate

import (
	"encoding/json"
	"strings"
	"testing"
)

// The channel request bundle-smoke-metadata.test.mjs admits through the packaging validator
func desktopChannelRequest(mutate func(map[string]any)) []byte {
	token := "0123456789abcdef"
	request := map[string]any{"schema": 1, "buildId": strings.Repeat("ab", 16), "channel": "smoke-" + token,
		"repository": "NousResearch/hermes-agent", "publicBase": "https://releases.example.test", "commit": strings.Repeat("a", 40),
		"sourceVersion": "1.2.3", "sequence": 65537, "version": "0.0.65537", "windowsVersion": "0.1.1.0", "bundleEnv": map[string]any{},
		"identity": map[string]any{"token": token, "displayName": "Smoke Channel", "appId": "com.example.preview-" + token,
			"appNamePascal": "Smoke" + token, "artifactNamePascal": "Artifact" + token, "msixAppIdWithOrg": "Example.Smoke" + token,
			"cliName": "smoke-" + token, "windowsExecutableName": "smoke-" + token}}
	if mutate != nil {
		mutate(request)
	}
	raw, _ := json.Marshal(request)
	return raw
}

func TestDesktopChannelRequestAdmitsTheSmokeRequestAndItsNativeConstraints(t *testing.T) {
	if _, err := ValidateDesktopChannelRequest(desktopChannelRequest(nil)); err != nil {
		t.Fatal(err)
	}
	identity := func(field, value string) func(map[string]any) {
		return func(r map[string]any) { r["identity"].(map[string]any)[field] = value }
	}
	for _, tc := range []struct {
		mutate func(map[string]any)
		want   string
	}{
		{identity("cliName", "con"), "reserved channel native identity cliName"},
		{identity("windowsExecutableName", "LPT1.smoke"), "reserved channel native identity windowsExecutableName"},
		{identity("appNamePascal", "Aux"), "reserved channel native identity appNamePascal"},
		{identity("artifactNamePascal", "com7"), "reserved channel native identity artifactNamePascal"},
		{identity("windowsExecutableName", "smoke."), "invalid channel native identity windowsExecutableName"},
		{func(r map[string]any) { r["bundleEnv"] = map[string]any{"ICEBATH_MODEL": "other"} }, "Bundle environment name is not permitted: ICEBATH_MODEL"},
		{func(r map[string]any) { r["bundleEnv"] = map[string]any{"ICEBATH_HOME": 1} }, ""},
		{func(r map[string]any) { r["commit"] = "b" }, ""},
	} {
		_, err := ValidateDesktopChannelRequest(desktopChannelRequest(tc.mutate))
		if err == nil || (tc.want != "" && err.Error() != tc.want) {
			t.Fatalf("want %q got %v", tc.want, err)
		}
	}
	if _, err := ValidateDesktopChannelRequest(desktopChannelRequest(func(r map[string]any) { r["bundleEnv"] = map[string]any{"ICEBATH_HOME": nil} })); err != nil {
		t.Fatalf("an explicit clear is admitted: %v", err)
	}
	if _, err := ValidateDesktopChannelRequest([]byte(`{"schema":1,"schema":1}`)); err == nil {
		t.Fatal("duplicate keys admitted")
	}
	if _, err := ValidateBundleEnv([]any{}); err == nil || err.Error() != "Bundle environment must be a JSON object" {
		t.Fatalf("non object %v", err)
	}
}
