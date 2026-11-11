package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Home under dir with config written when given, the _home of the hermes cases
func webappListenerHome(t *testing.T, dir, config string) string {
	h := filepath.Join(dir, "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func TestTheWebappStaysOffUntilConfigTurnsItOn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	off := WebappSettingsOf(context.Background(), webappListenerHome(t, t.TempDir(), ""))
	if host, port := off.Bind(); off.Enabled || host != "127.0.0.1" || port != 0 {
		t.Fatalf("off = %+v", off)
	}
}

func TestAnEnabledWebappBindsItsConfiguredAddress(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	on := WebappSettingsOf(context.Background(), webappListenerHome(t, t.TempDir(), "webapp:\n  enabled: true\n  host: 0.0.0.0\n  port: 9555\n"))
	if host, port := on.Bind(); !on.Enabled || host != "0.0.0.0" || port != 9555 {
		t.Fatalf("on = %+v", on)
	}
	def := WebappSettingsOf(context.Background(), webappListenerHome(t, t.TempDir(), "webapp:\n  enabled: true\n"))
	if host, port := def.Bind(); host != "127.0.0.1" || port != 9120 {
		t.Fatalf("default = %s %d", host, port)
	}
}

func TestAMalformedWebappBlockKeepsTheListenerPrivate(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, block := range []string{
		"webapp: yes\n",
		"webapp:\n  enabled: true\n  port: 70000\n",
		"webapp:\n  enabled: true\n  port: http\n",
		"webapp:\n  enabled: true\n  host: ''\n",
		"webapp:\n  enabled: 'true'\n",
		"webapp:\n  enabled: true\n  host: 192.168.1.20\n",
		"webapp:\n  enabled: true\n  host: hermes.example.net\n",
	} {
		settings := WebappSettingsOf(context.Background(), webappListenerHome(t, t.TempDir(), block))
		if host, port := settings.Bind(); host != "127.0.0.1" || port != 0 {
			t.Fatalf("%q binds %s %d", block, host, port)
		}
	}
}

func TestNativeClientsDialLoopbackWhateverTheBrowserBind(t *testing.T) {
	for bind, dial := range map[string]string{"127.0.0.1": "127.0.0.1", "::1": "::1", "0.0.0.0": "127.0.0.1", "::": "::1"} {
		if got := WebappDialHost(bind); got != dial {
			t.Fatalf("%s dials %s", bind, got)
		}
	}
}

func TestTheAdvertisedAPIOriginIsTheLoopbackDialAddress(t *testing.T) {
	for bind, origin := range map[string]string{"127.0.0.1": "http://127.0.0.1:9120", "0.0.0.0": "http://127.0.0.1:9120",
		"::": "http://[::1]:9120", "::1": "http://[::1]:9120"} {
		if got := WebappDialOrigin(bind, 9120); got != origin {
			t.Fatalf("%s origin %s", bind, got)
		}
	}
}
