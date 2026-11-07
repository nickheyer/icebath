package plugins

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs a Relay binding whose validate_exact answers through validate, removed when the test ends
func installRelayValidator(t *testing.T, validate func(payload map[string]any) (map[string]any, error)) {
	t.Helper()
	tools.RegisterNemoRelayBinding(func() (*tools.NemoRelay, error) {
		return &tools.NemoRelay{Plugin: &tools.NemoRelayPlugin{ValidateExact: func(_ context.Context, payload map[string]any) (map[string]any, error) {
			return validate(payload)
		}}}, nil
	})
	t.Cleanup(func() { tools.RegisterNemoRelayBinding(nil) })
}

func TestRelayBindingBridgesFailLikeTheMissingWheel(t *testing.T) {
	tools.RegisterNemoRelayBinding(nil)
	t.Cleanup(func() { tools.RegisterNemoRelayBinding(nil) })
	if _, err := ValidateRelayPluginPayload(context.Background(), relayObject("version", 1)); relayExceptionText(err) != "ModuleNotFoundError: No module named 'nemo_relay'" {
		t.Fatalf("validate without binding %v", err)
	}
	if _, err := RelayPluginPayloadFromLegacyEnv(map[string]any{}); relayExceptionText(err) != "ModuleNotFoundError: No module named 'nemo_relay'" {
		t.Fatalf("payload without binding %v", err)
	}
	tools.RegisterNemoRelayBinding(func() (*tools.NemoRelay, error) { return nil, nil })
	if _, err := loadNemoRelayBinding(); relayExceptionText(err) != "ModuleNotFoundError: No module named 'nemo_relay'" {
		t.Fatalf("nil binding %v", err)
	}
	broken := &tools.RelayError{Class: "ImportError", Message: "libnemo_relay.so: cannot open shared object file"}
	tools.RegisterNemoRelayBinding(func() (*tools.NemoRelay, error) { return nil, broken })
	if _, err := ValidateRelayPluginPayload(context.Background(), relayObject("version", 1)); relayExceptionText(err) != "ImportError: libnemo_relay.so: cannot open shared object file" {
		t.Fatalf("broken binding %v", err)
	}
	for _, c := range []struct {
		relay *tools.NemoRelay
		want  string
	}{
		{nil, "ModuleNotFoundError: No module named 'nemo_relay'"},
		{&tools.NemoRelay{}, "AttributeError: 'NemoRelay' object has no attribute 'plugin'"},
		{&tools.NemoRelay{Plugin: &tools.NemoRelayPlugin{}}, "AttributeError: 'NemoRelayPlugin' object has no attribute 'validate_exact'"},
	} {
		if _, err := relayValidateExact(context.Background(), c.relay, map[string]any{"version": 1}); relayExceptionText(err) != c.want {
			t.Fatalf("%+v %v", c.relay, err)
		}
	}
}

func TestRelayEnvBridgesKeepTheEnvFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("\ufeffA=1\r\nexport B=2"), 0o640); err != nil {
		t.Fatal(err)
	}
	lines, err := relayEnvReadLines(path)
	if err != nil || !reflect.DeepEqual(lines, []string{"A=1\n", "export B=2\n"}) {
		t.Fatalf("lines %q %v", lines, err)
	}
	if !relayEnvLineDefinesKey(lines[1], "B") || relayEnvLineDefinesKey(lines[0], "B") {
		t.Fatalf("defines key")
	}
	if err := relayEnvWriteLines(path, []string{"# migrated\n", "C=3\n"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	data, _ := os.ReadFile(path)
	if err != nil || info.Mode().Perm() != 0o640 || string(data) != "# migrated\nC=3\n" {
		t.Fatalf("written %q %v %v", data, info, err)
	}
}

// A diagnostic that is not a mapping fails the d.get call before any error level message joins
func TestRelayNonMappingDiagnosticRaisesAttributeError(t *testing.T) {
	installRelayValidator(t, func(map[string]any) (map[string]any, error) {
		return relayReport(map[string]any{"level": "error", "message": "rejected"}, "loose"), nil
	})
	_, err := ValidateRelayPluginPayload(context.Background(), relayObject("version", 1))
	if err == nil || relayExceptionText(err) != "AttributeError: 'str' object has no attribute 'get'" {
		t.Fatalf("err %v", err)
	}
}
