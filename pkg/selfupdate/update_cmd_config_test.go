package selfupdate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

func TestMigrationPolicy(t *testing.T) {
	type call struct{ interactive, quiet bool }
	cases := []struct {
		name     string
		expected []call
	}{
		{"format", []call{{false, true}}}, {"warnings", []call{{false, true}}}, {"migration-error", []call{{false, true}}},
		{"current", nil}, {"ahead", nil}, {"read-error", nil}, {"yes", []call{{false, false}}}, {"tty-yes", []call{{true, false}}},
		{"tty-decline", nil}, {"noninteractive", []call{{false, false}}}, {"gateway", []call{{false, false}}}, {"eof", nil}, {"unicode", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.name
			t.Setenv("ICEBATH_HOME", t.TempDir())
			var prompts []string
			var calls []call
			named := map[string]bool{"yes": true, "tty-yes": true, "tty-decline": true, "noninteractive": true, "gateway": true, "eof": true, "unicode": true}[name]
			savedEnv, savedFields, savedVersion, savedMigrate, savedSibling := configMissingEnvVars, configMissingFields, configCheckVersion, configMigrate, configMigrationSibling
			savedIn, savedOut, savedInput, savedGateway := updateStdinIsTTY, updateStdoutIsTTY, updateInput, updateGatewayPrompt
			t.Cleanup(func() {
				configMissingEnvVars, configMissingFields, configCheckVersion, configMigrate, configMigrationSibling = savedEnv, savedFields, savedVersion, savedMigrate, savedSibling
				updateStdinIsTTY, updateStdoutIsTTY, updateInput, updateGatewayPrompt = savedIn, savedOut, savedInput, savedGateway
			})
			configMissingEnvVars = func(ctx context.Context, h string, requiredOnly bool) ([]config.MissingEnvVar, error) {
				if !requiredOnly {
					t.Fatal("the check asked for optional settings")
				}
				if named {
					return []config.MissingEnvVar{{Name: "NEW_TOKEN", EnvVar: config.EnvVar{Description: "new credential"}}}, nil
				}
				return nil, nil
			}
			configMissingFields = func(context.Context, string, config.Lookup) ([]config.MissingConfigField, error) {
				if named {
					return []config.MissingConfigField{{Key: "new.option", Description: "new option"}}, nil
				}
				return nil, nil
			}
			configCheckVersion = func(ctx context.Context, h string, raiseOnParseError bool) (int, int, error) {
				if !raiseOnParseError {
					t.Fatal("the version check did not ask to raise on a parse error")
				}
				switch name {
				case "read-error":
					return 0, 0, &pm.PyError{Class: "RuntimeError", Message: "cannot read config"}
				case "ahead":
					return 4, 3, nil
				case "current":
					return 3, 3, nil
				}
				return 2, 3, nil
			}
			configMigrate = func(ctx context.Context, h string, interactive, quiet bool) (*config.MigrationResults, error) {
				calls = append(calls, call{interactive, quiet})
				if name == "migration-error" {
					return nil, &pm.PyError{Class: "RuntimeError", Message: "cannot write config"}
				}
				results := &config.MigrationResults{}
				if name == "warnings" {
					results.ConfigAdded, results.Warnings = []string{"setting reset"}, []string{"personality reset"}
				}
				return results, nil
			}
			configMigrationSibling = func(context.Context, map[string][]string) ([]siblingMigration, error) { return nil, nil }
			updateStdinIsTTY = func() bool { return name != "noninteractive" }
			updateStdoutIsTTY = func(io.Writer) bool { return name != "noninteractive" }
			updateInput = func(text string) (string, error) {
				prompts = append(prompts, text)
				switch name {
				case "eof":
					return "", cliui.ErrEOF
				case "unicode":
					return "", &pm.PyError{Class: "UnicodeDecodeError", Message: "'utf-8' codec can't decode byte 0xff in position 0: invalid"}
				case "tty-decline":
					return "n", nil
				}
				return "y", nil
			}
			var gatewayPrompts [][2]string
			updateGatewayPrompt = func(ctx context.Context, w io.Writer, text, def string, timeout float64) (string, error) {
				gatewayPrompts = append(gatewayPrompts, [2]string{text, def})
				return "y", nil
			}
			var out bytes.Buffer
			err := checkAndApplyConfigMigration(context.Background(), &out, name == "yes", name == "gateway", nil)
			if name == "migration-error" {
				if err == nil || pm.ErrorClass(err) != "RuntimeError" || !strings.Contains(err.Error(), "v2 → v3 was not written: cannot write config") {
					t.Fatalf("err = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, tc.expected) {
				t.Fatalf("migrate calls = %v, want %v", calls, tc.expected)
			}
			wantPrompt := map[string]bool{"tty-yes": true, "tty-decline": true, "eof": true, "unicode": true}[name]
			if (len(prompts) > 0) != wantPrompt {
				t.Fatalf("prompts = %v", prompts)
			}
			if (len(gatewayPrompts) > 0) != (name == "gateway") {
				t.Fatalf("gateway prompts = %v", gatewayPrompts)
			}
			output := out.String()
			has := func(parts ...string) {
				t.Helper()
				for _, part := range parts {
					if !strings.Contains(output, part) {
						t.Fatalf("output lacks %q:\n%s", part, output)
					}
				}
			}
			switch {
			case named:
				has("NEW_TOKEN", "new.option", "new credential", "new option")
				if name == "yes" || name == "noninteractive" || name == "gateway" {
					has("API keys require manual entry")
				} else if len(tc.expected) == 0 {
					has("icebath config migrate")
				}
			case name == "current" || name == "ahead":
				has("Configuration is up to date")
			case name == "read-error":
				has("Could not check config version")
			case name == "migration-error":
				has("Config format update failed: cannot write config")
			default:
				has("v2 → v3", "no new settings to configure")
				if name == "warnings" {
					has("setting reset", "personality reset")
				}
			}
		})
	}
}

// Writes a profile home whose config carries a format version and a model provider
func ucWriteProfile(t *testing.T, root, name string, version int) string {
	t.Helper()
	h := filepath.Join(root, name)
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("_config_version: %d\nmodel:\n  provider: nous\n", version)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

// Config format version a profile home's file carries on disk
func ucDiskVersion(t *testing.T, h string) int {
	t.Helper()
	current, _, err := config.CheckConfigVersion(home.WithOverride(context.Background(), h), h, true)
	if err != nil {
		t.Fatal(err)
	}
	return current
}

// Model provider a profile home's config carries on disk
func ucDiskProvider(t *testing.T, h string) any {
	t.Helper()
	cfg, err := config.LoadContext(home.WithOverride(context.Background(), h), h, nil)
	if err != nil {
		t.Fatal(err)
	}
	model, _ := cfg["model"].(map[string]any)
	return model["provider"]
}

// Names of the migrated siblings in order
func ucMigratedNames(migrated []siblingMigration) []string {
	names := []string{}
	for _, m := range migrated {
		names = append(names, m.Name)
	}
	return names
}

// Runs the sibling sweep with active as the process home
func ucSiblingSweep(t *testing.T, active string) []siblingMigration {
	t.Helper()
	t.Setenv("ICEBATH_HOME", active)
	migrated, err := migrateSiblingProfileConfigs(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return migrated
}

func TestSiblingBehindIsMigratedOnDisk(t *testing.T) {
	root := t.TempDir()
	latest := config.LatestConfigVersion()
	active := ucWriteProfile(t, filepath.Join(root, "profiles"), "active", latest)
	sibling := ucWriteProfile(t, filepath.Join(root, "profiles"), "research", 12)
	migrated := ucSiblingSweep(t, active)
	var entry *siblingMigration
	for i := range migrated {
		if migrated[i].Name == "research" {
			entry = &migrated[i]
		}
	}
	if entry == nil || entry.From != 12 || entry.To != latest {
		t.Fatalf("migrated = %+v", migrated)
	}
	if got := ucDiskVersion(t, sibling); got != latest {
		t.Fatalf("on disk version = %d", got)
	}
	if got := ucDiskProvider(t, sibling); got != "nous" {
		t.Fatalf("provider = %v", got)
	}
}

func TestActiveProfileIsSkipped(t *testing.T) {
	root := t.TempDir()
	active := ucWriteProfile(t, filepath.Join(root, "profiles"), "active", 12)
	migrated := ucSiblingSweep(t, active)
	for _, name := range ucMigratedNames(migrated) {
		if name == "active" {
			t.Fatal("the active profile was migrated by the sibling sweep")
		}
	}
	if got := ucDiskVersion(t, active); got != 12 {
		t.Fatalf("active version = %d", got)
	}
}

func TestCurrentSiblingUntouched(t *testing.T) {
	root := t.TempDir()
	latest := config.LatestConfigVersion()
	active := ucWriteProfile(t, filepath.Join(root, "profiles"), "active", latest)
	sibling := ucWriteProfile(t, filepath.Join(root, "profiles"), "work", latest)
	before, _ := os.ReadFile(filepath.Join(sibling, "config.yaml"))
	if migrated := ucSiblingSweep(t, active); len(migrated) != 0 {
		t.Fatalf("migrated = %+v", migrated)
	}
	if after, _ := os.ReadFile(filepath.Join(sibling, "config.yaml")); !bytes.Equal(before, after) {
		t.Fatal("a current sibling's config was rewritten")
	}
}

func TestUnconfiguredProfileSkipped(t *testing.T) {
	root := t.TempDir()
	active := ucWriteProfile(t, filepath.Join(root, "profiles"), "active", config.LatestConfigVersion())
	bare := filepath.Join(root, "profiles", "empty")
	if err := os.Mkdir(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	if migrated := ucSiblingSweep(t, active); len(migrated) != 0 {
		t.Fatalf("migrated = %+v", migrated)
	}
	if _, err := os.Stat(filepath.Join(bare, "config.yaml")); err == nil {
		t.Fatal("an unconfigured profile gained a config")
	}
}

func TestOneBrokenProfileDoesNotBlockOthers(t *testing.T) {
	root := t.TempDir()
	active := ucWriteProfile(t, filepath.Join(root, "profiles"), "active", config.LatestConfigVersion())
	broken := filepath.Join(root, "profiles", "broken")
	if err := os.Mkdir(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(broken, "config.yaml"), []byte(":\nnot yaml: ["), 0o644)
	ucWriteProfile(t, filepath.Join(root, "profiles"), "healthy", 12)
	if got := ucMigratedNames(ucSiblingSweep(t, active)); !reflect.DeepEqual(got, []string{"healthy"}) {
		t.Fatalf("migrated = %v", got)
	}
}

func TestOverrideIsResetAfterRun(t *testing.T) {
	root := t.TempDir()
	active := ucWriteProfile(t, filepath.Join(root, "profiles"), "active", config.LatestConfigVersion())
	ucWriteProfile(t, filepath.Join(root, "profiles"), "research", 12)
	t.Setenv("ICEBATH_HOME", active)
	ctx := context.Background()
	before := home.Override(ctx)
	if _, err := migrateSiblingProfileConfigs(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if after := home.Override(ctx); after != before {
		t.Fatalf("override leaked: %q", after)
	}
	if got := home.Dir(ctx); got != active {
		t.Fatalf("home after the sweep = %q", got)
	}
}

func TestNamedActiveMigratesDefaultOutsideProfiles(t *testing.T) {
	root := t.TempDir()
	latest := config.LatestConfigVersion()
	defaultHome := ucWriteProfile(t, root, "default-home", 12)
	active := ucWriteProfile(t, filepath.Join(defaultHome, "profiles"), "work", latest)
	migrated := ucSiblingSweep(t, active)
	var entry *siblingMigration
	for i := range migrated {
		if migrated[i].Name == "default" {
			entry = &migrated[i]
		}
	}
	if entry == nil || entry.From != 12 || entry.To != latest {
		t.Fatalf("migrated = %+v", migrated)
	}
	if got := ucDiskVersion(t, defaultHome); got != latest {
		t.Fatalf("default version = %d", got)
	}
	if got := ucDiskProvider(t, defaultHome); got != "nous" {
		t.Fatalf("provider = %v", got)
	}
	for _, name := range ucMigratedNames(migrated) {
		if name == "work" {
			t.Fatal("the named active profile was migrated by the sibling sweep")
		}
	}
}

func TestDefaultActiveStillMigratesNamedSiblings(t *testing.T) {
	root := t.TempDir()
	latest := config.LatestConfigVersion()
	defaultHome := ucWriteProfile(t, root, "default-home", latest)
	sibling := ucWriteProfile(t, filepath.Join(defaultHome, "profiles"), "research", 12)
	names := ucMigratedNames(ucSiblingSweep(t, defaultHome))
	if len(names) != 1 || names[0] != "research" {
		t.Fatalf("migrated = %v", names)
	}
	if got := ucDiskVersion(t, sibling); got != latest {
		t.Fatalf("sibling version = %d", got)
	}
	if got := ucDiskVersion(t, defaultHome); got != latest {
		t.Fatalf("default version = %d", got)
	}
}

func TestSiblingStepNoticesReachTheUpdateOutput(t *testing.T) {
	root := t.TempDir()
	active := ucWriteProfile(t, filepath.Join(root, "profiles"), "active", config.LatestConfigVersion())
	work := filepath.Join(root, "profiles", "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(work, "config.yaml"), []byte("_config_version: 51\nbrowser:\n  backend: off\n"), 0o644)
	t.Setenv("ICEBATH_HOME", active)
	savedEnv, savedFields := configMissingEnvVars, configMissingFields
	t.Cleanup(func() { configMissingEnvVars, configMissingFields = savedEnv, savedFields })
	configMissingEnvVars = func(context.Context, string, bool) ([]config.MissingEnvVar, error) { return nil, nil }
	configMissingFields = func(context.Context, string, config.Lookup) ([]config.MissingConfigField, error) {
		return []config.MissingConfigField{{Key: "display.new_option"}}, nil
	}
	savedIn := updateStdinIsTTY
	updateStdinIsTTY = func() bool { return false }
	t.Cleanup(func() { updateStdinIsTTY = savedIn })
	var buf bytes.Buffer
	if err := checkAndApplyConfigMigration(context.Background(), &buf, false, false, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	profileLine := strings.Index(out, "Profile 'work': config format updated (v51")
	notice := strings.Index(out, "removed browser.backend: off")
	if profileLine < 0 || notice < 0 || profileLine >= notice {
		t.Fatalf("profile line %d, notice %d:\n%s", profileLine, notice, out)
	}
	if !strings.Contains(out[notice:], "`browser` toolset in `icebath tools`") {
		t.Fatalf("notice lacks the toolset hint:\n%s", out[notice:])
	}
	if strings.Contains(out[profileLine:], "display.new_option") {
		t.Fatal("a new default key was printed as a change")
	}
	text, _ := os.ReadFile(filepath.Join(work, "config.yaml"))
	if strings.Contains(string(text), "backend") {
		t.Fatalf("browser.backend survived the migration:\n%s", text)
	}
}
