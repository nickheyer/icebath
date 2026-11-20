package acpclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func vendorVendor() Vendor {
	v := BaseVendor()
	v.DefaultCommand, v.DefaultArgs = "vendor-agent", []string{"--acp"}
	v.CommandEnvVars, v.ArgsEnvVar = []string{"VENDOR_ACP_COMMAND", "VENDOR_CLI_PATH"}, "VENDOR_ACP_ARGS"
	v.MarkerBaseURL, v.PlaceholderModel, v.Label = "acp://vendor", "vendor-acp", "Vendor ACP"
	v.ModelEntryDisabled = func(entry any) (bool, error) {
		return dictGet(or(dictGet(entry, "_meta"), map[string]any{}), "vendorPolicy") == "off", nil
	}
	v.EarlyExitError = func(stderrText string) (string, bool) {
		if strings.Contains(stderrText, "legacy build") {
			return "vendor CLI too old: " + stderrText, true
		}
		return "", false
	}
	return v
}

// Client of the vendor launching the test binary as the fake agent in the mode
func fakeAgentClient(t *testing.T, v Vendor, mode, tmp string) *Client {
	t.Helper()
	t.Setenv("ACPCLIENT_FAKE", mode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(context.Background(), v, Options{Command: exe, Args: []string{"fake"}, ACPCwd: tmp})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// First choice message of a completion map
func completionMessage(t *testing.T, completion any) map[string]any {
	t.Helper()
	m, _ := completion.(map[string]any)
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in %#v", completion)
	}
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	return message
}

func userHi(content string) []any {
	return []any{map[string]any{"role": "user", "content": content}}
}

func TestSubclassLaunchFallbackReadsItsEnvVarsInOrder(t *testing.T) {
	tmp := hermeticHome(t)
	for _, name := range []string{"VENDOR_ACP_COMMAND", "VENDOR_CLI_PATH", "VENDOR_ACP_ARGS"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	ctx := context.Background()
	c, err := New(ctx, vendorVendor(), Options{ACPCwd: tmp})
	if err != nil || c.acpCommand != "vendor-agent" || !reflect.DeepEqual(c.acpArgs, []string{"--acp"}) || c.BaseURL() != "acp://vendor" || c.APIKey() != "vendor-acp" {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("VENDOR_CLI_PATH", "/opt/second")
	t.Setenv("VENDOR_ACP_COMMAND", "/opt/first")
	t.Setenv("VENDOR_ACP_ARGS", "--acp --stdio")
	c, err = New(ctx, vendorVendor(), Options{ACPCwd: tmp})
	if err != nil || c.acpCommand != "/opt/first" || !reflect.DeepEqual(c.acpArgs, []string{"--acp", "--stdio"}) {
		t.Fatalf("%q %q %v", c.acpCommand, c.acpArgs, err)
	}
	c, err = New(ctx, vendorVendor(), Options{Command: "x", Args: []string{"y"}, ACPCwd: tmp})
	if err != nil || !reflect.DeepEqual(c.acpArgs, []string{"y"}) {
		t.Fatalf("%q %v", c.acpArgs, err)
	}
}

func TestModelEntryDisabledHookOwnsVendorFiltering(t *testing.T) {
	tmp := hermeticHome(t)
	ctx := context.Background()
	generic := fakeAgentClient(t, BaseVendor(), "catalog", tmp)
	vendor := fakeAgentClient(t, vendorVendor(), "catalog", tmp)
	models, err := generic.ListModels(ctx, 30)
	if err != nil || !reflect.DeepEqual(models, []string{"open-model", "policy-blocked", "copilot-gated"}) {
		t.Fatalf("generic %q %v", models, err)
	}
	models, err = vendor.ListModels(ctx, 30)
	if err != nil || !reflect.DeepEqual(models, []string{"open-model", "copilot-gated"}) {
		t.Fatalf("vendor %q %v", models, err)
	}
	completion, err := vendor.CreateChatCompletion(ctx, map[string]any{"model": "vendor-acp", "messages": userHi("hi"), "timeout": 30})
	if err != nil {
		t.Fatal(err)
	}
	if content := completionMessage(t, completion)["content"]; content != "hello" || completion.(map[string]any)["model"] != "vendor-acp" {
		t.Fatalf("%#v", completion)
	}
}

func TestErrorsCarryTheSubclassLabelAndEarlyExitHook(t *testing.T) {
	tmp := hermeticHome(t)
	ctx := context.Background()
	missing, err := New(ctx, vendorVendor(), Options{Command: filepath.Join(tmp, "absent"), Args: []string{"--stdio"}, ACPCwd: tmp})
	if err != nil {
		t.Fatal(err)
	}
	_, err = missing.CreateChatCompletion(ctx, map[string]any{"messages": userHi("x"), "timeout": 10})
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != "RuntimeError" || !regexp.MustCompile(`Could not start Vendor ACP command .*VENDOR_ACP_COMMAND/VENDOR_CLI_PATH`).MatchString(pe.Msg) {
		t.Fatalf("%v", err)
	}
	_, err = fakeAgentClient(t, vendorVendor(), "legacy", tmp).ListModels(ctx, 30)
	if !errors.As(err, &pe) || pe.Kind != "RuntimeError" || !strings.Contains(pe.Msg, "vendor CLI too old: legacy build 1.0") {
		t.Fatalf("%v", err)
	}
	_, err = fakeAgentClient(t, BaseVendor(), "legacy", tmp).ListModels(ctx, 30)
	if !errors.As(err, &pe) || pe.Kind != "RuntimeError" || !strings.Contains(pe.Msg, "ACP process exited early: legacy build 1.0") {
		t.Fatalf("%v", err)
	}
}

func TestMalformedVendorModelOptionsFallBackToTheSessionDefault(t *testing.T) {
	tmp := hermeticHome(t)
	t.Setenv("ACPCLIENT_FAKE_MALFORMED", "1")
	completion, err := fakeAgentClient(t, vendorVendor(), "catalog", tmp).CreateChatCompletion(context.Background(),
		map[string]any{"model": "open-model", "messages": userHi("hi"), "timeout": 30})
	if err != nil {
		t.Fatal(err)
	}
	if content := completionMessage(t, completion)["content"]; content != "hello" {
		t.Fatalf("%#v", completion)
	}
}
