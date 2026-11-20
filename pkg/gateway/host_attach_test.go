package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Claims the host lock and writes the owner record as raw JSON with no served set proof
func publishRaw(t *testing.T, pid int, home string, served []string) {
	t.Helper()
	if o, _ := ClaimHostLock(context.Background(), RoleGateway); o != HostLockAcquired {
		t.Fatal(o)
	}
	payload := pyjson.NewObject()
	payload.Set("role", RoleGateway)
	payload.Set("home", home)
	payload.Set("pid", int64(pid))
	if ct := HostProcessCreateTime(pid); ct != nil {
		payload.Set("createTime", *ct)
	} else {
		payload.Set("createTime", nil)
	}
	payload.Set("host", "")
	payload.Set("port", nil)
	payload.Set("protocolVersion", int64(HostProtocolVersion))
	payload.Set("tokenFingerprint", "")
	list := []any{}
	for _, p := range served {
		list = append(list, p)
	}
	payload.Set("profiles", list)
	payload.Set("updatedAt", "")
	data, err := payload.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(HostRecordPath(RoleGateway)), 0o700)
	if err := os.WriteFile(HostRecordPath(RoleGateway), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func setHostControl(t *testing.T, identify, rescan func(string, time.Duration) *pyjson.Object) {
	t.Helper()
	prevID, prevRescan, prevWait := hostIdentifyGateway, hostRescanGatewayProfiles, attachChannelWait
	if identify != nil {
		hostIdentifyGateway = identify
	}
	if rescan != nil {
		hostRescanGatewayProfiles = rescan
	}
	t.Cleanup(func() {
		hostIdentifyGateway, hostRescanGatewayProfiles, attachChannelWait = prevID, prevRescan, prevWait
	})
}

func TestARecordOnlyOwnerNeverProducesAttach(t *testing.T) {
	dir := hostDir(t)
	owner := startHostChild(t, "sleep")
	ours := filepath.Join(dir, "root", "profiles", "other")
	publishRaw(t, owner.pid(), filepath.Join(dir, "root"), []string{"default", "other"})
	silent := func(string, time.Duration) *pyjson.Object { return nil }
	setHostControl(t, silent, silent)
	attachChannelWait = 0
	d, err := DecideHostAttach(t.Context(), ours, false)
	if err != nil || d.Outcome != HostAttachRefuse || !d.Transient || d.Owner == nil || d.Owner.ServedKnown {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestReplaceStillTargetsAnOwnerWhoseServedSetIsNotKnownYet(t *testing.T) {
	dir := hostDir(t)
	owner := startHostChild(t, "sleep")
	ownerHome := filepath.Join(dir, "root")
	publishRaw(t, owner.pid(), ownerHome, nil)
	d, err := DecideHostAttach(t.Context(), filepath.Join(ownerHome, "profiles", "other"), true)
	if err != nil || d.Outcome != HostAttachReplaceHost || d.Owner == nil || d.Owner.PID != owner.pid() {
		t.Fatalf("%+v %v", d, err)
	}
}
