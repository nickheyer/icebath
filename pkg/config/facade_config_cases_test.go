package config

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Migration facade home holding the config text and .env text, stdout and stderr captured
func caseHome(t *testing.T, configText, envText string) (string, string, func() (string, string)) {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(EnvManagedDir, filepath.Join(t.TempDir(), "no-managed"))
	InvalidateManagedCache()
	quietParseFailures(t)
	ResetCache()
	t.Cleanup(ResetCache)
	out, errOut := captureFacade(t)
	path := home.ConfigPath(h)
	if configText != "" {
		os.WriteFile(path, []byte(configText), 0o644)
	}
	os.WriteFile(home.EnvPath(h), []byte(envText), 0o644)
	return h, path, func() (string, string) {
		o, e := out.String(), errOut.String()
		out.Reset()
		errOut.Reset()
		return o, e
	}
}

func loadCase(t *testing.T, h string) Tree {
	t.Helper()
	ResetCache()
	cfg, err := Load(h, facadeLookup)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestGLMSuffixCollisionNotSplit(t *testing.T) {
	lines := []string{"GLM_API_KEY=glm-secret\n", "GLM_BASE_URL=https://api.z.ai/api/paas/v4\n"}
	if got := SanitizeEnvLines(lines); !reflect.DeepEqual(got, lines) {
		t.Fatalf("GLM lines corrupted %q", got)
	}
}

func TestSanitizeEnvFileKeepsValueSemanticsAndCleanFiles(t *testing.T) {
	for _, text := range []string{"FAL_KEY=good\nOPENROUTER_API_KEY=valFIRECRAWL_API_KEY=val2\n", "GOOD_KEY=good\nOTHER_KEY=other\n"} {
		h, _, _ := caseHome(t, "", text)
		fixes, err := SanitizeEnvFile(context.Background(), h)
		data, _ := os.ReadFile(home.EnvPath(h))
		if err != nil || fixes != 0 || string(data) != text {
			t.Fatalf("fixes %d err %v content %q", fixes, err, data)
		}
	}
}

func TestConfigCommandExitsCleanlyOnBrokenYAML(t *testing.T) {
	original := "model:\n  default: keep\nbroken: [unterminated\n"
	value := "gpt-4o"
	for _, args := range []ConfigArgs{{Command: "set", Key: "model.default", Value: &value}, {Command: "unset", Key: "model.default"}} {
		h, path, output := caseHome(t, original, "")
		err := ConfigCommand(context.Background(), h, args, pathCommandSources())
		_, errOut := output()
		data, _ := os.ReadFile(path)
		if exitCode(err) != 1 || !strings.Contains(errOut, "formatting error") || string(data) != original {
			t.Fatalf("%s returned %v stderr %q file %q", args.Command, err, errOut, data)
		}
		if args.Command == "set" && !strings.Contains(errOut, "`icebath config edit`") {
			t.Fatalf("set stderr %q", errOut)
		}
	}
}
