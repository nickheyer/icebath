package updaterecord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func prOwedIDs(t *testing.T) []string {
	t.Helper()
	found, err := PauseOrphans(context.Background(), "")
	if err != nil {
		return nil
	}
	var ids []string
	for _, f := range found {
		ids = append(ids, fmt.Sprint(pauseTokenOf(f.Body)["pause_id"]))
	}
	return ids
}

func prOwedProfiles(t *testing.T) []map[string]any {
	t.Helper()
	found, err := PauseOrphans(context.Background(), "")
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, f := range found {
		out = append(out, mapOf(pauseTokenOf(f.Body)["profiles"]))
	}
	return out
}

func prUnreadable(t *testing.T, path, how string) {
	t.Helper()
	if how == "malformed" {
		if err := os.WriteFile(path, []byte("{trunc"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
}

func prSkipRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
}

func TestPauseOurOwnPIDIsOursOnlyAtOurExactCreationTime(t *testing.T) {
	me := PauseIdentity(0)
	ct, _ := me["ct"].(string)
	if !strings.HasPrefix(ct, "ct:") || !PauseIdentityIsLive(me) {
		t.Fatalf("identity %v", me)
	}
	created, err := strconv.ParseFloat(ct[3:], 64)
	if err != nil {
		t.Fatal(err)
	}
	earlier := created - 0.5
	if earlier == float64(int64(earlier)) {
		earlier = created - 0.75
	}
	reused := map[string]any{"pid": os.Getpid(), "ct": fmt.Sprintf("ct:%.3f", earlier)}
	if PauseIdentityIsLive(reused) {
		t.Fatal("a killed update's record became ours by pid reuse")
	}
}

func TestPauseRecordNamingOurPIDWithoutCreationTimeIsAPreviousIncarnation(t *testing.T) {
	if PauseIdentityIsLive(map[string]any{"pid": os.Getpid(), "ct": nil}) {
		t.Fatal("a ct-less record naming our pid counted as live")
	}
}

func TestPauseHostWithoutPsutilRecordsAnIdentityTheLivenessRuleCanProve(t *testing.T) {
	ident := PauseIdentity(0)
	if ident["ct"] == nil {
		t.Fatal("no creation time recorded")
	}
	if live := IncarnationLive(ident["pid"].(int), ident["ct"]); live == nil || !*live {
		t.Fatalf("liveness of our own identity %v", live)
	}
}

func TestPauseUnreadableRetiredListClaimsNothingAndForgetsNothing(t *testing.T) {
	prSkipRoot(t)
	psHome(t)
	ctx := context.Background()
	src := PauseRecordPath()
	retired := pauseRetiredPath(src)
	done := strings.Repeat("a", 32)
	if err := WritePause(ctx, map[string]any{"pause_id": done, "resume_needed": true, "profiles": map[string]any{"default": 4242}}, PauseUnowned(), ""); err != nil {
		t.Fatal(err)
	}
	if err := pauseAtomicWrite(retired, map[string]any{"schema": 1, "ids": []any{done}}); err != nil {
		t.Fatal(err)
	}
	if ids := prOwedIDs(t); len(ids) != 0 {
		t.Fatalf("owed %v", ids)
	}
	other := filepath.Join(filepath.Dir(src), filepath.Base(src)+".999.deadbeef.claim")
	if err := pauseAtomicWrite(other, map[string]any{"install_root": PauseInstallRoot(), "claimer": PauseUnowned(),
		"token": map[string]any{"pause_id": strings.Repeat("b", 32)}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(retired, 0); err != nil {
		t.Fatal(err)
	}
	func() {
		defer os.Chmod(retired, 0o644)
		for _, id := range prOwedIDs(t) {
			if id == done {
				t.Fatal("a completed obligation is owed again while its list is unreadable")
			}
		}
		if _, _, ok, err := ClaimPause(ctx, src); ok || err != nil {
			t.Fatalf("a completed obligation was claimed again %v", err)
		}
		mctx, release, err := PauseMutex(ctx, PauseMutexWaitSeconds)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := ReadPauseRecord(other)
		err = retirePause(src, []PauseFile{{other, body}})
		release()
		_ = mctx
		if err == nil || !isOSError(err) {
			t.Fatalf("retire under an unreadable list %v", err)
		}
		if !pathExists(other) {
			t.Fatal("a retirement that could not be recorded deleted its carrier")
		}
	}()
	raw, _ := os.ReadFile(retired)
	v, _ := pyjson.Loads(string(raw))
	if !reflect.DeepEqual(pyjson.Plain(v).(map[string]any)["ids"], []any{done}) {
		t.Fatalf("the history was rewritten %s", raw)
	}
	for _, id := range prOwedIDs(t) {
		if id == done {
			t.Fatal("completed obligation owed")
		}
	}
}

func TestPauseUnreadableNewerCarrierNeverLetsItsOlderCopyExecute(t *testing.T) {
	prSkipRoot(t)
	for _, how := range []string{"refused", "malformed"} {
		t.Run(how, func(t *testing.T) {
			psHome(t)
			ctx := context.Background()
			src := PauseRecordPath()
			pauseID := strings.Repeat("d", 32)
			if err := WritePause(ctx, map[string]any{"pause_id": pauseID, "resume_needed": true,
				"profiles": map[string]any{"alpha": 11, "beta": 12}}, PauseUnowned(), ""); err != nil {
				t.Fatal(err)
			}
			newer := filepath.Join(filepath.Dir(src), filepath.Base(src)+".999.deadbeef.claim")
			if err := pauseAtomicWrite(newer, map[string]any{"install_root": PauseInstallRoot(), "claimer": PauseUnowned(), "rev": 1,
				"token": map[string]any{"pause_id": pauseID, "resume_needed": true, "profiles": map[string]any{"beta": 12}}}); err != nil {
				t.Fatal(err)
			}
			if owed := prOwedProfiles(t); !reflect.DeepEqual(owed, []map[string]any{{"beta": int64(12)}}) {
				t.Fatalf("premise: the furthest-progressed copy executes, owed %v", owed)
			}
			prUnreadable(t, newer, how)
			func() {
				defer os.Chmod(newer, 0o644)
				for _, p := range prOwedProfiles(t) {
					if len(p) == 2 {
						t.Fatal("completed alpha would be restarted again")
					}
				}
				if _, _, ok, err := ClaimPause(ctx, src); ok || err != nil {
					t.Fatalf("the older copy was claimed past an unreadable newer one %v", err)
				}
			}()
			if !pathExists(src) || !pathExists(newer) {
				t.Fatal("a carrier was deleted while its state was unknown")
			}
		})
	}
}

func TestPauseUnreadableOrphanIsNeverOverwrittenByANewPause(t *testing.T) {
	prSkipRoot(t)
	for _, how := range []string{"refused", "malformed"} {
		t.Run(how, func(t *testing.T) {
			psHome(t)
			ctx := context.Background()
			src := PauseRecordPath()
			if err := WritePause(ctx, map[string]any{"pause_id": strings.Repeat("e", 32), "resume_needed": true,
				"profiles": map[string]any{"alpha": 11}}, PauseUnowned(), ""); err != nil {
				t.Fatal(err)
			}
			saved, _ := os.ReadFile(src)
			prUnreadable(t, src, how)
			err := WritePause(ctx, map[string]any{"pause_id": strings.Repeat("f", 32), "resume_needed": true,
				"profiles": map[string]any{"beta": 12}}, PauseIdentity(0), "")
			os.Chmod(src, 0o644)
			if err == nil || !isOSError(err) {
				t.Fatalf("write over an unknown record %v", err)
			}
			now, _ := os.ReadFile(src)
			if how == "refused" && string(now) != string(saved) {
				t.Fatal("alpha's saved restart obligation was replaced")
			}
			if how == "malformed" && string(now) != "{trunc" {
				t.Fatal("an unknown record was replaced")
			}
			if err := os.WriteFile(src, saved, 0o644); err != nil {
				t.Fatal(err)
			}
			token := map[string]any{"pause_id": strings.Repeat("f", 32), "resume_needed": true, "profiles": map[string]any{"beta": 12}}
			if err := WritePause(ctx, token, PauseIdentity(0), ""); err != nil {
				t.Fatal(err)
			}
			body, err := ReadPauseRecord("")
			if err != nil || !reflect.DeepEqual(mapOf(pauseTokenOf(body)["profiles"]), map[string]any{"alpha": int64(11), "beta": int64(12)}) {
				t.Fatalf("merged profiles %v %v", body, err)
			}
		})
	}
}

func TestPauseRefusedPublishLeavesNoTempBesideTheRecord(t *testing.T) {
	target := filepath.Join(t.TempDir(), "records", "record.json")
	pauseReplace = func(src, dst string) error {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: os.ErrPermission}
	}
	t.Cleanup(func() { pauseReplace = os.Rename })
	if err := pauseAtomicWrite(target, map[string]any{"schema": 1}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("publish error %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 0 {
		t.Fatalf("the half-published temp was left behind %v", entries)
	}
}

func TestPauseMalformedRetiredListIsSetAsideOnlyWhenNothingRefersToIt(t *testing.T) {
	psHome(t)
	ctx := context.Background()
	retired := pauseRetiredPath(PauseRecordPath())
	if err := os.WriteFile(retired, []byte("{trunc"), 0o644); err != nil {
		t.Fatal(err)
	}
	adopted, claims, err := AdoptPauseOrphans(ctx)
	if err != nil || adopted != nil || len(claims) != 0 {
		t.Fatalf("a corrupt history aborted the update %v %v %v", adopted, claims, err)
	}
	if corrupt, _ := os.ReadFile(pyWithSuffix(retired, ".corrupt")); pathExists(retired) || string(corrupt) != "{trunc" {
		t.Fatal("malformed history not set aside")
	}
	if err := WritePause(ctx, map[string]any{"pause_id": strings.Repeat("c", 32), "resume_needed": true,
		"profiles": map[string]any{"default": 4242}}, PauseUnowned(), ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retired, []byte("{trunc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PauseOrphans(ctx, ""); !IsPauseRecordError(err, "RetiredUnknown") {
		t.Fatalf("orphans under a referenced malformed list %v", err)
	}
	if raw, _ := os.ReadFile(retired); string(raw) != "{trunc" {
		t.Fatal("referenced malformed history moved")
	}
}
