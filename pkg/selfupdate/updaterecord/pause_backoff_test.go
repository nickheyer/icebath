package updaterecord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

func pbResumer(t *testing.T, resume func(ctx context.Context, token map[string]any, stdout io.Writer) error) {
	saved := pauseGatewayResumer
	SetPauseGatewayResumer(resume)
	t.Cleanup(func() { pauseGatewayResumer = saved })
}

func pbArgv() { pm.SetCommandArgv(func(argv []string) []string { return argv }) }

func pbProfiles(token map[string]any) []string {
	var names []string
	for name := range mapOf(token["profiles"]) {
		names = append(names, name)
	}
	return sortedKeys(strSetOf(names))
}

func strSetOf(items []string) map[string]bool {
	set := map[string]bool{}
	for _, item := range items {
		set[item] = true
	}
	return set
}

func pbRetry(t *testing.T, claim string) map[string]any {
	t.Helper()
	body, err := ReadPauseRecord(claim)
	if err != nil || body == nil {
		t.Fatalf("claim unreadable: %v", err)
	}
	return mapOf(mapOf(mapOf(body["token"])["relaunch_retry"])["profile:alpha"])
}

func TestPauseRecoverAnUnreadyRelaunchStallsOneLaunchPerBackoffWindowAndStaysOwed(t *testing.T) {
	psHome(t)
	pbArgv()
	ctx := context.Background()
	token, err := StampTree(ctx, map[string]any{"resume_needed": true, "profiles": map[string]any{"alpha": 4194000}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePause(ctx, token, PauseUnowned(), ""); err != nil {
		t.Fatal(err)
	}
	var handed [][]string
	pbResumer(t, func(ctx context.Context, token map[string]any, stdout io.Writer) error {
		handed = append(handed, pbProfiles(token))
		fmt.Fprintln(stdout, "relaunched; waiting for readiness")
		return errors.New("Windows gateway relaunch after update was not verified alive: alpha")
	})
	var out, errOut bytes.Buffer
	savedOut, savedErr := pauseStdout, pauseStderr
	pauseStdout, pauseStderr = &out, &errOut
	t.Cleanup(func() { pauseStdout, pauseStderr = savedOut, savedErr })
	for _, argv := range [][]string{{"status"}, {"chat"}} {
		if err := RecoverPauses(ctx, argv); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(handed, [][]string{{"alpha"}}) {
		t.Fatalf("a launch inside the backoff waited on the unready relaunch again: %v", handed)
	}
	claims := pauseClaims(PauseRecordPath())
	if len(claims) != 1 {
		t.Fatalf("claims = %v", claims)
	}
	body, _ := ReadPauseRecord(claims[0])
	if profiles := mapOf(mapOf(body["token"])["profiles"]); len(profiles) != 1 || !pyrepr.Equal(profiles["alpha"], 4194000) {
		t.Fatalf("the unready gateway's restart debt was dropped: %v", profiles)
	}
	if retry := pbRetry(t, claims[0]); !pyrepr.Equal(retry["attempts"], 1) {
		t.Fatalf("attempts = %v", retry["attempts"])
	}
	if strings.Contains(out.String(), "waiting for readiness") || !strings.Contains(errOut.String(), "waiting for readiness") {
		t.Fatalf("recovery wrote into the stdout of an unrelated command: out=%q err=%q", out.String(), errOut.String())
	}
	raw, err := os.ReadFile(claims[0])
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	plain := pyjson.Plain(loaded).(map[string]any)
	mapOf(mapOf(mapOf(plain["token"])["relaunch_retry"])["profile:alpha"])["next_at"] = 0
	text, err := pyjson.Dumps(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claims[0], []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RecoverPauses(ctx, []string{"status"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(handed, [][]string{{"alpha"}, {"alpha"}}) {
		t.Fatalf("the debt was not retried once its backoff elapsed: %v", handed)
	}
	claims = pauseClaims(PauseRecordPath())
	if len(claims) != 1 {
		t.Fatalf("claims = %v", claims)
	}
	if retry := pbRetry(t, claims[0]); !pyrepr.Equal(retry["attempts"], 2) {
		t.Fatalf("attempts = %v", retry["attempts"])
	}
}
