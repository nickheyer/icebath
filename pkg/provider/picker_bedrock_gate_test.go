package provider

import (
	"context"
	"testing"
)

// Offline picker with no AWS env and a counter on the full credential chain probe
func bedrockGatePicker(t *testing.T) (context.Context, *int) {
	t.Helper()
	ctx, _ := pickerTestHome(t, nil)
	unsetEnvForTest(t, "AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_BEARER_TOKEN_BEDROCK",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI")
	saved := pickerAWSCredentialsFn
	t.Cleanup(func() { pickerAWSCredentialsFn = saved })
	calls := 0
	pickerAWSCredentialsFn = func(bool) bool {
		calls++
		return false
	}
	return ctx, &calls
}

func TestBedrockNotShownWithoutCredentials(t *testing.T) {
	ctx, _ := bedrockGatePicker(t)
	if row := pickerRow(mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openai"}), "bedrock"); row != nil {
		t.Fatalf("bedrock listed without AWS credentials %v", row)
	}
}

func TestNonBedrockPickerDoesNotProbeFullAWSChain(t *testing.T) {
	ctx, calls := bedrockGatePicker(t)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openrouter", MaxModels: intPtr(0)})
	if *calls != 0 || pickerRow(rows, "bedrock") != nil {
		t.Fatalf("aws chain probed %d times, rows %v", *calls, rowSlugs(rows))
	}
}
