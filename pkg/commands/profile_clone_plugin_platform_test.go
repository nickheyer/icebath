package commands

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
)

// Compiled-in platform plugin registering sharedplat with the SHP_ shared prefix, the sharedplat fixture plugin
type sharedPlatPlugin struct{}

func (sharedPlatPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "sharedplat", Kind: "platform", Version: "1.0.0",
		RequiresEnv: []plugins.EnvSpec{{Name: "SHP_TOKEN", Secret: true}}}
}

func (sharedPlatPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{Name: "sharedplat", Label: "Shared",
		AdapterFactory: gateway.AdapterFactory(func(context.Context, *gateway.PlatformConfig) (any, error) { return nil, nil }),
		Check:          func() bool { return true }, RequiredEnv: []string{"SHP_TOKEN"},
		Extra: map[string]any{"shared_env_prefixes": []string{"SHP_"}}})
	return err
}

func TestPluginPlatformSharedEnvPrefixesFollowTheOwnershipRule(t *testing.T) {
	for _, c := range []struct{ enabled, stripped bool }{{false, false}, {true, true}} {
		t.Run(map[bool]string{false: "False-False", true: "True-True"}[c.enabled], func(t *testing.T) {
			tmp := profileTestHome(t)
			def := filepath.Join(tmp, ".icebath")
			var out strings.Builder
			t.Cleanup(manage.SwapOutput(&out, &out))
			t.Cleanup(plugins.SwapBundled(sharedPlatPlugin{}))
			saved := plugins.InstalledServices()
			svc := &plugins.Services{}
			if saved != nil {
				*svc = *saved
			}
			svc.Platforms = gateway.PlatformRegistryService()
			plugins.InstallServices(svc)
			t.Cleanup(func() { plugins.InstallServices(saved) })
			writeTestFile(t, filepath.Join(def, ".env"), []byte("OPENAI_API_KEY=sk\nSHP_TOKEN=tool-token\nSHP_URL=http://x\n"))
			enabled := map[bool]string{true: "true", false: "false"}[c.enabled]
			writeTestFile(t, filepath.Join(def, "config.yaml"), []byte("model:\n  default: gpt-5\n  provider: openai\n"+
				"platforms:\n  sharedplat:\n    enabled: "+enabled+"\nplugins:\n  enabled:\n  - sharedplat\n"))
			dir, err := manage.CreateProfile(context.Background(), "shp"+map[bool]string{true: "1", false: "0"}[c.enabled], manage.CreateOptions{CloneConfig: true})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, ".env"))
			if err != nil {
				t.Fatal(err)
			}
			keys := []string{}
			for _, line := range pystr.Splitlines(string(raw), false) {
				if strings.Contains(line, "=") && !strings.HasPrefix(line, "#") {
					keys = append(keys, strings.SplitN(line, "=", 2)[0])
				}
			}
			sort.Strings(keys)
			want := []string{"OPENAI_API_KEY", "SHP_TOKEN", "SHP_URL"}
			if c.stripped {
				want = []string{"OPENAI_API_KEY"}
			}
			if !reflect.DeepEqual(keys, want) {
				t.Fatalf("keys %v want %v\n%s", keys, want, out.String())
			}
		})
	}
}
