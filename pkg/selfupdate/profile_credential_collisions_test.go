package selfupdate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
)

const collisionSecret = "123456:shared-secret-value"

// default and worker profile homes with the real migration seams pointed at an empty host
func collisionHomes(t *testing.T) (string, string) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	worker := filepath.Join(root, "profiles", "worker")
	if err := os.MkdirAll(worker, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(tmp, "locks"))
	t.Setenv(home.EnvHome, root)
	for _, name := range []string{"TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "OPENAI_API_KEY"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	profiles.ResetFileCache()
	profiles.ResetStandaloneMemo()
	home.ResetKeyCache()
	saved := gatewayrun.MigrateS6
	t.Cleanup(func() { gatewayrun.MigrateS6 = saved })
	gatewaycli.InstallMigrateS6Hooks()
	gatewayrun.MigrateS6.Running = func() bool { return false }
	return root, worker
}

// Output of the doctor profile check followed by the gateway status warnings
func collisionSurfaces(t *testing.T, root string) string {
	var out bytes.Buffer
	d := &Doctor{W: &out, Home: root, ProjectRoot: t.TempDir(), DisplayHome: root}
	if _, err := checkProfiles(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	gatewaycli.PrintDuplicateCredentialWarnings(context.Background(), &out)
	return out.String()
}

func TestDuplicateSingletonSecretNamedIdenticallyByDoctorStatusAndPreflight(t *testing.T) {
	root, worker := collisionHomes(t)
	env := "TELEGRAM_BOT_TOKEN=" + collisionSecret + "\nOPENAI_API_KEY=sk-shared\n"
	writeTestFile(t, filepath.Join(root, ".env"), env, 0o644)
	writeTestFile(t, filepath.Join(worker, ".env"), env, 0o644)
	ctx := context.Background()
	findings, err := gatewayrun.DuplicateCredentialFindings(ctx)
	if err != nil || len(findings) != 1 {
		t.Fatalf("findings %v %v", findings, err)
	}
	line := findings[0]
	for _, want := range []string{"'default'", "'worker'", "TELEGRAM_BOT_TOKEN", gatewayrun.MigrateCommand} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
	plan, err := gatewayrun.BuildMigrationPlan(ctx)
	if err != nil || !reflect.DeepEqual(plan.Blockers, findings) {
		t.Fatalf("blockers %v %v", plan.Blockers, err)
	}
	out := collisionSurfaces(t, root)
	if strings.Count(out, line) != 2 {
		t.Fatalf("line count %d\n%s", strings.Count(out, line), out)
	}
	for _, leaked := range []string{collisionSecret, "shared-secret", "sk-shared"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("%q reached a surface", leaked)
		}
	}
}

func TestDistinctTokensAndSharedNonSingletonKeysAreNotFindings(t *testing.T) {
	root, worker := collisionHomes(t)
	writeTestFile(t, filepath.Join(root, ".env"), "TELEGRAM_BOT_TOKEN="+collisionSecret+"\nOPENAI_API_KEY=sk-shared\n", 0o644)
	writeTestFile(t, filepath.Join(worker, ".env"), "TELEGRAM_BOT_TOKEN=999999:other-token\nOPENAI_API_KEY=sk-shared\n", 0o644)
	ctx := context.Background()
	if findings, err := gatewayrun.DuplicateCredentialFindings(ctx); err != nil || len(findings) != 0 {
		t.Fatalf("findings %v %v", findings, err)
	}
	if plan, err := gatewayrun.BuildMigrationPlan(ctx); err != nil || plan.Blocked() {
		t.Fatalf("plan blocked %v", err)
	}
	if strings.Contains(collisionSurfaces(t, root), "both hold") {
		t.Fatal("distinct tokens reported as duplicates")
	}
}
