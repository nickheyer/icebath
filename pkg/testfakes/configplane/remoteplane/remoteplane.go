package remoteplane

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/remote"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane"
)

// Write check, path codec and secret check of the remote backend for the plane
func Rules() configplane.Rules {
	return configplane.Rules{
		Decode: func(encoded string) ([]string, error) {
			p, err := remote.Decode(encoded)
			return []string(p), err
		},
		Encode: remote.Encode,
		WriteCheck: func(setPaths [][]string, setValues []any, unsets [][]string, locks []configplane.Lock) (configplane.Lock, bool) {
			sets := &remote.PathMap{}
			for i, p := range setPaths {
				sets.Set(remote.Path(p), setValues[i])
			}
			us := make([]remote.Path, len(unsets))
			for i, u := range unsets {
				us[i] = remote.Path(u)
			}
			ls := make([]remote.Lock, len(locks))
			for i, l := range locks {
				ls[i] = remote.Lock{Path: remote.Path(l.Path), Level: l.Level}
			}
			lock, refused := remote.WriteCheck(sets, us, ls)
			return configplane.Lock{Path: []string(lock.Path), Level: lock.Level}, refused
		},
		SecretLiteral: func(path []string, value any) bool {
			_, bad := remote.SecretLiteralPath(remote.Path(path), value)
			return bad
		},
	}
}

// Remote deployment env pointing at the plane with the IdP client credentials
func Env(p *configplane.Plane) map[string]string {
	return map[string]string{
		config.BackendEnv:                 "remote",
		remote.URLEnv:                     p.URL,
		remote.InstanceEnv:                configplane.Instance,
		"GATEWAY_RELAY_IDP_TOKEN_URL":     p.URL + "/token",
		"GATEWAY_RELAY_IDP_CLIENT_ID":     configplane.ClientID,
		"GATEWAY_RELAY_IDP_CLIENT_SECRET": configplane.ClientSecret,
	}
}

// Plane fixture with a fresh home, the remote env, zero boot delays and a fresh backend on both ends
func Setup(t testing.TB) *configplane.Plane {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	p := configplane.New(t, Rules())
	for k, v := range Env(p) {
		t.Setenv(k, v)
	}
	saved := remote.BootRetryDelays
	remote.BootRetryDelays = []float64{0, 0}
	remote.ResetForTests()
	p.Home = h
	t.Cleanup(func() {
		remote.ResetForTests()
		remote.BootRetryDelays = saved
	})
	return p
}
