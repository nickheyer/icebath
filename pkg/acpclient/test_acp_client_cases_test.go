package acpclient

import (
	"context"
	"os"
	"reflect"
	"testing"
)

func vendorVendor() Vendor {
	v := BaseVendor()
	v.DefaultCommand, v.DefaultArgs = "vendor-agent", []string{"--acp"}
	v.CommandEnvVars, v.ArgsEnvVar = []string{"VENDOR_ACP_COMMAND", "VENDOR_CLI_PATH"}, "VENDOR_ACP_ARGS"
	v.MarkerBaseURL, v.PlaceholderModel, v.Label = "acp://vendor", "vendor-acp", "Vendor ACP"
	return v
}

func TestSubclassLaunchFallbackReadsItsEnvVarsInOrder(t *testing.T) {
	tmp := hermeticHome(t)
	for _, name := range []string{"VENDOR_ACP_COMMAND", "VENDOR_CLI_PATH", "VENDOR_ACP_ARGS"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	ctx := context.Background()
	c, err := New(ctx, vendorVendor(), Options{ACPCwd: tmp})
	if err != nil || c.acpCommand != "vendor-agent" || !reflect.DeepEqual(c.acpArgs, []string{"--acp"}) || c.BaseURL != "acp://vendor" || c.APIKey != "vendor-acp" {
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
