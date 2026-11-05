package commands

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/spf13/cobra"
)

func TestPMEveryPathBound(t *testing.T) {
	c := NewCLI()
	for _, path := range c.Unbound() {
		if path == "pm" || strings.HasPrefix(path, "pm ") {
			t.Errorf("pm path %q has no handler", path)
		}
	}
	for _, path := range []string{"pm", "pm lock", "pm install", "pm env", "pm doctor", "pm repair", "pm environment",
		"pm gc", "pm bundle", "pm status", "pm update", "pm worker", "pm stage-native"} {
		if c.find(path) == nil {
			t.Errorf("pm path %q missing", path)
		}
		if _, ok := c.handlers[path]; !ok {
			t.Errorf("pm path %q unbound", path)
		}
	}
	for _, path := range []string{"pm worker", "pm stage-native", "update-takeover"} {
		if cmd := c.find(path); cmd == nil || !cmd.Hidden {
			t.Errorf("%q is not a hidden command", path)
		}
	}
}

// Parses argv through the full CLI with the path's handler swapped for one capturing the built arguments
func pmParse(t *testing.T, path string, argv []string, build func(cmd *cobra.Command, args []string) any) (any, error) {
	t.Helper()
	c := NewCLI()
	var got any
	c.Bind(path, func(cmd *cobra.Command, args []string) error {
		got = build(cmd, args)
		return nil
	})
	err := c.Execute(argv)
	return got, err
}

func ptr(s string) *string { return &s }

func TestPMFlagsParse(t *testing.T) {
	lock := func(cmd *cobra.Command, args []string) any { return pmLockArgs(cmd, args) }
	install := func(cmd *cobra.Command, args []string) any { return pmInstallArgs(cmd, args) }
	environment := func(cmd *cobra.Command, args []string) any { return pmEnvironmentArgs(cmd, args) }
	bundle := func(cmd *cobra.Command, _ []string) any { return pmBundleArgs(cmd) }
	update := func(cmd *cobra.Command, args []string) any { return pmUpdateArgs(cmd, args) }
	stage := func(cmd *cobra.Command, _ []string) any { return pmStageNativeArgs(cmd) }
	env := func(_ *cobra.Command, args []string) any { return append([]string{}, args...) }
	doctor := func(cmd *cobra.Command, _ []string) any { repair, _ := cmd.Flags().GetBool("repair"); return repair }
	cases := []struct {
		path  string
		argv  []string
		build func(cmd *cobra.Command, args []string) any
		want  any
	}{
		{"pm lock", []string{"pm", "lock"}, lock, pm.LockArgs{}},
		{"pm lock", []string{"pm", "lock", "--bump", "uv", "0.9.1"}, lock, pm.LockArgs{Name: ptr("uv"), Version: ptr("0.9.1")}},
		{"pm install", []string{"pm", "install"}, install, pm.InstallArgs{Names: []string{}, Extra: []string{}, Without: []string{}}},
		{"pm install", []string{"pm", "install", "node", "uv", "--extra", "matrix", "--extra", "voice", "--without", "chromium",
			"--tools-only", "--trust-recorded", "--target", "linux-arm64-bionic"}, install,
			pm.InstallArgs{Names: []string{"node", "uv"}, Extra: []string{"matrix", "voice"}, Without: []string{"chromium"},
				ToolsOnly: true, TrustRecorded: true, Target: ptr("linux-arm64-bionic")}},
		{"pm install", []string{"pm", "install", "--test-environment"}, install, pm.InstallArgs{Names: []string{}, Extra: []string{}, Without: []string{}, TestEnvironment: ptr("")}},
		{"pm install", []string{"pm", "install", "--test-environment", "dev,voice"}, install, pm.InstallArgs{Names: []string{}, Extra: []string{}, Without: []string{}, TestEnvironment: ptr("dev,voice")}},
		{"pm install", []string{"pm", "install", "--target", ""}, install, pm.InstallArgs{Names: []string{}, Extra: []string{}, Without: []string{}, Target: ptr("")}},
		{"pm env", []string{"pm", "env", "node", "python"}, env, []string{"node", "python"}},
		{"pm doctor", []string{"pm", "doctor", "--repair"}, doctor, true},
		{"pm environment", []string{"pm", "environment", "side", "--extra", "voice", "--requirement", "rich<14"}, environment,
			pm.EnvironmentArgs{Name: "side", Extra: []string{"voice"}, Requirement: []string{"rich<14"}}},
		{"pm bundle", []string{"pm", "bundle", "--out", "dist", "--ref", "v1", "--cache", "/c"}, bundle, pm.BundleArgs{Out: "dist", Ref: "v1", Cache: "/c"}},
		{"pm update", []string{"pm", "update", "node", "--check", "--target", "win32-arm64", "--skip-target", "linux-arm64-bionic",
			"--uv", "--npm", "--termux"}, update, pm.UpdateArgs{Names: []string{"node"}, Check: true, Target: "win32-arm64",
			SkipTarget: []string{"linux-arm64-bionic"}, UV: true, NPM: true, Termux: true}},
		{"pm stage-native", []string{"pm", "stage-native", "--out", "o", "--source", "s", "--tui", "t"}, stage,
			pm.StageNativeArgs{Out: "o", Ref: "HEAD", Source: "s", TUI: "t"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			got, err := pmParse(t, tc.path, tc.argv, tc.build)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}
}

func TestPMUsageErrors(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"pm"}, "icebath pm: error: the following arguments are required: cmd"},
		{[]string{"pm", "lock", "--bump", "uv"}, "icebath pm lock: error: --bump NAME and VERSION go together; run with neither to relock uv.lock"},
		{[]string{"pm", "lock", "0.9.1"}, "icebath pm lock: error: --bump NAME and VERSION go together; run with neither to relock uv.lock"},
		{[]string{"pm", "update", "--skip-target", "mars-x64"}, "argument --skip-target: invalid choice: 'mars-x64'"},
		{[]string{"pm", "bundle"}, `required flag(s) "out" not set`},
		{[]string{"pm", "environment"}, "the following arguments are required: name"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			err := NewCLI().Execute(tc.argv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v want %q", err, tc.want)
			}
			var exit *ExitError
			if strings.Contains(tc.want, ": error: ") && (!errors.As(err, &exit) || exit.Code != 2) {
				t.Fatalf("error %#v is not a status 2 usage error", err)
			}
		})
	}
}
