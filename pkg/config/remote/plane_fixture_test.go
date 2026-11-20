package remote

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane"
)

// Write check, path codec and secret check of this package for the shared plane double
func planeRules() configplane.Rules {
	return configplane.Rules{
		Decode: func(encoded string) ([]string, error) {
			p, err := Decode(encoded)
			return []string(p), err
		},
		Encode: Encode,
		WriteCheck: func(setPaths [][]string, setValues []any, unsets [][]string, locks []configplane.Lock) (configplane.Lock, bool) {
			sets := &PathMap{}
			for i, p := range setPaths {
				sets.Set(Path(p), setValues[i])
			}
			us := make([]Path, len(unsets))
			for i, u := range unsets {
				us[i] = Path(u)
			}
			ls := make([]Lock, len(locks))
			for i, l := range locks {
				ls[i] = Lock{Path: Path(l.Path), Level: l.Level}
			}
			lock, refused := WriteCheck(sets, us, ls)
			return configplane.Lock{Path: []string(lock.Path), Level: lock.Level}, refused
		},
		SecretLiteral: func(path []string, value any) bool {
			_, bad := SecretLiteralPath(Path(path), value)
			return bad
		},
	}
}

func newStubPlane(t *testing.T) *configplane.Plane { return configplane.New(t, planeRules()) }

// Remote deployment env pointing at the stub with the IdP client credentials
func remoteEnv(t *testing.T, p *configplane.Plane) {
	t.Setenv(config.BackendEnv, "remote")
	t.Setenv(URLEnv, p.URL)
	t.Setenv(InstanceEnv, configplane.Instance)
	t.Setenv(idpEnvPrefix+"TOKEN_URL", p.URL+"/token")
	t.Setenv(idpEnvPrefix+"CLIENT_ID", configplane.ClientID)
	t.Setenv(idpEnvPrefix+"CLIENT_SECRET", configplane.ClientSecret)
}

func clearIDPCache() {
	idpMu.Lock()
	idpCache = map[idpCacheKey]idpCached{}
	idpMu.Unlock()
}

// Fixture of the Python plane fixture with a default home, the stub env, zero boot delays and a fresh backend
func setupPlane(t *testing.T) *configplane.Plane {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	p := newStubPlane(t)
	remoteEnv(t, p)
	saved := BootRetryDelays
	BootRetryDelays = []float64{0, 0}
	ResetForTests()
	clearIDPCache()
	p.Home = h
	t.Cleanup(func() {
		ResetForTests()
		clearIDPCache()
		BootRetryDelays = saved
	})
	return p
}

func testBackend() *remoteBackend { return GetRemoteBackend().(*remoteBackend) }
