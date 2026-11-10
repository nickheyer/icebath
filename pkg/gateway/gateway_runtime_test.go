//go:build !windows

package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypwd"
)

func readyPayload(h string) *pyjson.Object {
	v, err := pyjson.Loads(fmt.Sprintf(`{"runtime_protocol": 1, "state": "ready", "instance_id": "inst", "authority_epoch": 3,
		"capabilities": ["session-authority-v1"], "api_origin": "http://127.0.0.1:4321", "supervisor": "none",
		"served_profiles": [{"profile_id": %q, "home": %q}]}`, h, h))
	if err != nil {
		panic(err)
	}
	return v.(*pyjson.Object)
}

// Unix listener at home's gateway.sock answering identify with the current payload until the test ends
func serveIdentify(t *testing.T, h string, payload func() *pyjson.Object) func() {
	t.Helper()
	sock := filepath.Join(h, "gateway.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(sock, 0o600)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			bufio.NewReader(conn).ReadString('\n')
			body, _ := pyjson.Dumps(payload())
			fmt.Fprintf(conn, "{\"ok\": true, \"protocol\": 1, \"id\": 1, \"result\": %s}\n", body)
			conn.Close()
		}
	}()
	stop := func() { ln.Close(); wg.Wait(); os.Remove(sock) }
	t.Cleanup(stop)
	return stop
}

func discover(h string, timeout time.Duration) ObservedEndpoint {
	return DiscoverGatewayEndpoint(context.Background(), h, timeout)
}

func TestLiveDiscoveryDoesNotTreatAnUnusableOwnerAsAbsent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux control socket fixture")
	}
	root := filepath.Dir(shortControlHome(t))
	h := filepath.Join(root, "profile")
	if got := discover(h, time.Second); got.State != "absent" {
		t.Fatalf("absent %+v", got)
	}
	os.Mkdir(h, 0o700)
	identity, _ := filepath.EvalSymlinks(h)
	var mu sync.Mutex
	payload := readyPayload(identity)
	payload.Set("instance_id", "fixture-instance")
	payload.Set("authority_epoch", 1)
	payload.Set("api_origin", "http://127.0.0.1:41817")
	stop := serveIdentify(t, h, func() *pyjson.Object { mu.Lock(); defer mu.Unlock(); return payload.Copy() })
	ready := discover(h, time.Second)
	if ready.State != "ready" || ready.Endpoint.ProfileID != identity || ready.Endpoint.InstanceID != "fixture-instance" ||
		ready.Endpoint.APIOrigin != "http://127.0.0.1:41817" {
		t.Fatalf("ready %+v", ready)
	}
	for _, c := range []struct {
		key      string
		value    any
		expected string
	}{
		{"runtime_protocol", 99, "incompatible"},
		{"runtime_protocol", true, "incompatible"},
		{"served_profiles", []any{}, "inaccessible"},
		{"state", "draining", "draining"},
		{"api_origin", "http://secret:credential@127.0.0.1:41817", "inaccessible"},
		{"api_origin", "http://example.com:41817", "inaccessible"},
		{"authority_epoch", true, "inaccessible"},
		{"capabilities", []any{}, "incompatible"},
	} {
		mu.Lock()
		old, _ := payload.Get(c.key)
		payload.Set(c.key, c.value)
		mu.Unlock()
		observed := discover(h, time.Second)
		if observed.State != c.expected || strings.Contains(fmt.Sprintf("%+v", observed), "credential") {
			t.Fatalf("%s=%v: %+v", c.key, c.value, observed)
		}
		mu.Lock()
		payload.Set(c.key, old)
		mu.Unlock()
	}
	stop()
	pointer := filepath.Join(h, "gateway.sock.path")
	os.WriteFile(pointer, []byte(filepath.Join(root, "foreign.sock")), 0o666)
	os.Chmod(pointer, 0o666)
	if got := discover(h, time.Second); got.State != "inaccessible" {
		t.Fatalf("forged pointer %+v", got)
	}
}

func TestFifoControlPointerIsRejectedWithoutAWriter(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux fifo semantics")
	}
	h := filepath.Join(t.TempDir(), "fifo-profile")
	os.Mkdir(h, 0o700)
	if err := syscall.Mkfifo(filepath.Join(h, "gateway.sock.path"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan ObservedEndpoint, 1)
	go func() { done <- discover(h, 100*time.Millisecond) }()
	select {
	case r := <-done:
		if r.State != "inaccessible" || r.ReasonCode != "unsafe_control_pointer" {
			t.Fatalf("%+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("discovery blocked on a FIFO instead of rejecting its file type")
	}
}

func TestStartDecisionsDoNotTurnUncertaintyIntoAnotherOwner(t *testing.T) {
	for _, c := range []struct {
		o    RuntimeObservation
		want string
	}{
		{RuntimeObservation{State: "ready"}, "attach"},
		{RuntimeObservation{State: "starting"}, "wait-owner"},
		{RuntimeObservation{State: "draining"}, "wait-owner"},
		{RuntimeObservation{State: "incompatible"}, "reject-version"},
		{RuntimeObservation{State: "inaccessible"}, "reject-access"},
		{RuntimeObservation{State: "conflict"}, "reject-conflict"},
		{RuntimeObservation{State: "absent"}, "spawn-unmanaged"},
		{RuntimeObservation{State: "absent", InstalledService: true}, "start-service"},
		{RuntimeObservation{State: "absent", ServiceMayStart: true}, "wait-service"},
		{RuntimeObservation{State: "ready", UpdatePaused: true}, "wait-update"},
	} {
		if got, err := NextStartAction(c.o); err != nil || got != c.want {
			t.Fatalf("%+v: %q %v", c.o, got, err)
		}
	}
	if _, err := NextStartAction(RuntimeObservation{State: "invalid"}); err == nil || !strings.Contains(err.Error(), "unknown runtime state") {
		t.Fatal(err)
	}
}

func TestEndpointMatchesServedHomeUnderAnyCanonicalSpelling(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real-home")
	os.Mkdir(real, 0o755)
	served, _ := filepath.EvalSymlinks(real)
	canon, _ := CanonicalHome(served)
	link := filepath.Join(tmp, "link-home")
	os.Symlink(real, link)
	for _, h := range []string{real, served, canon, link} {
		d, err := endpointFromPayload(readyPayload(served), h, "")
		if err != nil || d.State != "ready" || d.Endpoint.ProfileID != served {
			t.Fatalf("%s: %+v %v", h, d, err)
		}
	}
	other := filepath.Join(tmp, "other-home")
	os.Mkdir(other, 0o755)
	for _, h := range []string{other, served + "-suffix"} {
		if d, _ := endpointFromPayload(readyPayload(served), h, ""); d.ReasonCode != "profile_mismatch" {
			t.Fatalf("%s: %+v", h, d)
		}
	}
}

func TestReadyEndpointCarriesTheOwnerBootCommit(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	os.Mkdir(h, 0o755)
	served, _ := filepath.EvalSymlinks(h)
	sha := "0123456789abcdef0123456789abcdef01234567"
	p := readyPayload(served)
	p.Set("code_sha", sha)
	if d, _ := endpointFromPayload(p, h, ""); d.Endpoint.CodeSHA != sha {
		t.Fatalf("%+v", d.Endpoint)
	}
	if d, _ := endpointFromPayload(readyPayload(served), h, ""); d.Endpoint.CodeSHA != "" {
		t.Fatalf("%+v", d.Endpoint)
	}
	p.Set("code_sha", []any{"x"})
	if d, _ := endpointFromPayload(p, h, ""); d.Endpoint.CodeSHA != "" {
		t.Fatalf("%+v", d.Endpoint)
	}
}

func overlayFixture(t *testing.T, overlayMode os.FileMode) (owner, overlay string) {
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	owner, overlay = filepath.Join(tmp, "owner"), filepath.Join(tmp, "task-home")
	os.Mkdir(owner, 0o700)
	os.Mkdir(overlay, 0o700)
	os.Chmod(overlay, overlayMode)
	lock := filepath.Join(owner, "gateway.lock")
	os.WriteFile(lock, nil, 0o600)
	os.Symlink(lock, filepath.Join(overlay, "gateway.lock"))
	return owner, overlay
}

func TestDiscoveryResolvesAPrivateOverlayLockToItsOwner(t *testing.T) {
	owner, overlay := overlayFixture(t, 0o700)
	var queried []string
	patch(t, &queryIdentifySeam, func(h string, _ time.Duration) (*pyjson.Object, error) {
		queried = append(queried, h)
		return readyPayload(owner), nil
	})
	result := discover(overlay, 2*time.Second)
	if result.State != "ready" || result.Endpoint == nil || result.Endpoint.ProfileID != owner {
		t.Fatalf("%+v", result)
	}
	if len(queried) != 1 || queried[0] != owner {
		t.Fatalf("queried %v", queried)
	}
	if got, _ := GatewayProfileHome(overlay); got != owner {
		t.Fatalf("profile home %s", got)
	}
}

func TestDiscoveryDoesNotFollowAnOverlayLockFromAWritableHome(t *testing.T) {
	owner, overlay := overlayFixture(t, 0o707)
	var queried []string
	patch(t, &queryIdentifySeam, func(h string, _ time.Duration) (*pyjson.Object, error) {
		queried = append(queried, h)
		return readyPayload(owner), nil
	})
	if result := discover(overlay, 2*time.Second); result.State != "inaccessible" {
		t.Fatalf("%+v", result)
	}
	if len(queried) != 1 || queried[0] != overlay {
		t.Fatalf("queried %v", queried)
	}
}

// Short owner temp root holding the profile's private hermes-gw directory
func ownerControlDir(t *testing.T, h string, mode os.FileMode) string {
	root, err := os.MkdirTemp("/tmp", "h-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	hash, _ := controlHomeHash(h)
	dir := filepath.Join(root, "hermes-gw-"+hash)
	os.Mkdir(dir, mode)
	os.Chmod(dir, mode)
	return dir
}

func TestFallbackPointerUsesOwnerLocationAndKeepsIdentityChecks(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profile")
	os.Mkdir(h, 0o700)
	dir := ownerControlDir(t, h, 0o700)
	target := filepath.Join(dir, "control.sock")
	pointer := filepath.Join(h, "gateway.sock.path")
	os.WriteFile(pointer, []byte(target), 0o600)
	ln, err := net.Listen("unix", target)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	os.Chmod(target, 0o600)
	requests := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		line, _ := bufio.NewReader(conn).ReadString('\n')
		requests <- line
		conn.Write([]byte(`{"ok":true,"protocol":1,"id":1,"result":{"state":"ready"}}` + "\n"))
		conn.Close()
	}()
	got, err := QueryIdentify(h, 2*time.Second)
	if err != nil || got.Len() != 1 || recGet(got, "state") != "ready" {
		t.Fatalf("%v %v", got, err)
	}
	if req := <-requests; !strings.Contains(req, `"verb":"identify"`) {
		t.Fatalf("request %q", req)
	}
	os.WriteFile(pointer, []byte(filepath.Join(filepath.Dir(dir), "hermes-gw-wrong", "control.sock")), 0o600)
	var de *DiscoveryError
	if _, err := QueryIdentify(h, 2*time.Second); !errors.As(err, &de) || de.Reason != "invalid_control_pointer" {
		t.Fatalf("wrong dir %v", err)
	}
	os.WriteFile(pointer, []byte(target), 0o600)
	os.Chmod(dir, 0o755)
	if _, err := QueryIdentify(h, 2*time.Second); !errors.As(err, &de) || de.Reason != "unsafe_control_permissions" {
		t.Fatalf("open dir %v", err)
	}
}

func TestTheClientVerdictNamesTheUnsafeNodeAndItsFix(t *testing.T) {
	h := shortControlHome(t)
	shown, _ := filepath.EvalSymlinks(h)
	control := filepath.Join(h, "gateway.sock")
	shownControl := filepath.Join(shown, "gateway.sock")
	os.WriteFile(control, []byte("not a socket"), 0o644)
	verdict := discover(h, 2*time.Second)
	if verdict.State != "inaccessible" || verdict.ReasonCode != "unsafe_control_path" ||
		!strings.Contains(verdict.Detail, shownControl+" is -rw") || !strings.Contains(verdict.Detail, "expected socket") {
		t.Fatalf("%+v", verdict)
	}
	os.Remove(control)
	ln, err := net.Listen("unix", control)
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(control, 0o660)
	verdict = discover(h, 2*time.Second)
	if verdict.ReasonCode != "unsafe_control_permissions" ||
		verdict.Detail != fmt.Sprintf("%s has mode 0660, expected 0600; chmod 600 %s", shownControl, shownControl) {
		t.Fatalf("%+v", verdict)
	}
	ln.Close()
	os.Remove(control)
	os.Chmod(h, 0o757)
	verdict = discover(h, 2*time.Second)
	if verdict.ReasonCode != "unsafe_control_permissions" ||
		verdict.Detail != fmt.Sprintf("%s has mode 0757, writable by another user; chmod go-w %s", shown, shown) {
		t.Fatalf("%+v", verdict)
	}
	os.Chmod(h, 0o700)
	info, _ := os.Lstat(h)
	owner := int(info.Sys().(*syscall.Stat_t).Uid)
	patch(t, &discoveryGetuid, func() int { return owner + 1 })
	verdict = discover(h, 2*time.Second)
	if verdict.ReasonCode != "unsafe_control_path" || verdict.Detail != fmt.Sprintf("%s is owned by uid %d, expected uid %d (gateway user); "+
		"chown it to the gateway user", shown, owner, owner+1) {
		t.Fatalf("%+v", verdict)
	}
}

func TestALongHomeKeepsTheRepairCommandWholeOrDropsIt(t *testing.T) {
	omitted := "; repair command omitted (path too long to print)"
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	h := filepath.Join(tmp, strings.Repeat("p", 200), "profile's home")
	os.MkdirAll(h, 0o700)
	os.Chmod(h, 0o757)
	if v := discover(h, 2*time.Second); v.ReasonCode != "unsafe_control_permissions" || v.Detail != h+" has mode 0757, writable by another user"+omitted {
		t.Fatalf("%+v", v)
	}
	deeper := filepath.Join(h, strings.Repeat("q", 250), strings.Repeat("r", 100))
	os.MkdirAll(deeper, 0o700)
	os.Chmod(deeper, 0o757)
	detail := discover(deeper, 2*time.Second).Detail
	if n := len([]rune(detail)); n != 500 || !strings.HasSuffix(detail, "… [truncated]"+omitted) {
		t.Fatalf("%d %q", n, detail)
	}
	if !strings.HasPrefix(detail, deeper[:300]) || strings.Contains(detail, "chmod") {
		t.Fatalf("%q", detail)
	}
}

func TestAPermissionHintRunsAsPrintedForAPathWithSpaces(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profile home")
	os.Mkdir(h, 0o700)
	control := filepath.Join(h, "control file")
	os.WriteFile(control, nil, 0o600)
	for _, c := range []struct {
		path, kind string
		isHome     bool
		mode       os.FileMode
	}{{control, "file", false, 0o660}, {h, "directory", true, 0o702}} {
		os.Chmod(c.path, c.mode)
		_, err := privateNode(c.path, c.kind, c.isHome)
		var de *DiscoveryError
		if !errors.As(err, &de) || de.Reason != "unsafe_control_permissions" {
			t.Fatalf("%s %v", c.path, err)
		}
		_, hint, _ := strings.Cut(de.Error(), "; ")
		if out, err := exec.Command("sh", "-c", hint).CombinedOutput(); err != nil {
			t.Fatalf("%s: %s %v", hint, out, err)
		}
		if _, err := privateNode(c.path, c.kind, c.isHome); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAnOpenControlDirectoryNamesItsPathAndMode(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profile")
	os.Mkdir(h, 0o700)
	dir := ownerControlDir(t, h, 0o755)
	os.WriteFile(filepath.Join(h, "gateway.sock.path"), []byte(filepath.Join(dir, "control.sock")), 0o600)
	_, err := QueryIdentify(h, 2*time.Second)
	var de *DiscoveryError
	if !errors.As(err, &de) || de.Reason != "unsafe_control_permissions" || de.Detail != fmt.Sprintf("%s has mode 0755, expected 0700; chmod 700 %s", dir, dir) {
		t.Fatalf("%v", err)
	}
}

func homeStat(t *testing.T, h string) *syscall.Stat_t {
	info, err := os.Lstat(h)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t)
}

func TestHomeModeRefusesOnlyWritesAnotherUserCanMake(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	os.Mkdir(h, 0o755)
	user := pypwd.Passwd{Name: "owner", UID: os.Getuid(), GID: os.Getgid()}
	patch(t, &discoveryGetpwuid, func(int) (pypwd.Passwd, error) { return user, nil })
	patch(t, &discoveryGetpwall, func() ([]pypwd.Passwd, error) { return []pypwd.Passwd{user}, nil })
	private := pypwd.Group{Name: user.Name, GID: user.GID}
	shared := pypwd.Group{Name: "staff", GID: user.GID, Mem: []string{user.Name, "someone-else"}}
	for _, c := range []struct {
		mode   os.FileMode
		group  pypwd.Group
		unsafe bool
	}{{0o700, private, false}, {0o755, shared, false}, {0o757, private, true}, {0o775, private, false}, {0o775, shared, true}} {
		os.Chmod(h, c.mode)
		g := c.group
		patch(t, &discoveryGetgrgid, func(int) (pypwd.Group, error) { return g, nil })
		if got := HomeModeUnsafe(homeStat(t, h), h); got != c.unsafe {
			t.Fatalf("%o %s: %v", c.mode, g.Name, got)
		}
	}
}

func TestGroupWriteIsPrivateOnlyWhenNoOtherAccountHasThatPrimaryGroup(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	os.Mkdir(h, 0o755)
	os.Chmod(h, 0o775)
	user := pypwd.Passwd{Name: "owner", UID: os.Getuid(), GID: os.Getgid()}
	patch(t, &discoveryGetpwuid, func(int) (pypwd.Passwd, error) { return user, nil })
	patch(t, &discoveryGetgrgid, func(int) (pypwd.Group, error) { return pypwd.Group{Name: user.Name, GID: user.GID}, nil })
	other := pypwd.Passwd{Name: "intruder", UID: user.UID + 1, GID: user.GID}
	patch(t, &discoveryGetpwall, func() ([]pypwd.Passwd, error) { return []pypwd.Passwd{user, other}, nil })
	if !HomeModeUnsafe(homeStat(t, h), h) {
		t.Fatal("shared primary group read as private")
	}
	patch(t, &discoveryGetpwall, func() ([]pypwd.Passwd, error) { return []pypwd.Passwd{user}, nil })
	if HomeModeUnsafe(homeStat(t, h), h) {
		t.Fatal("private group read as shared")
	}
	patch(t, &discoveryGetpwall, func() ([]pypwd.Passwd, error) { return nil, errors.New("passwd database unavailable") })
	if !HomeModeUnsafe(homeStat(t, h), h) {
		t.Fatal("unreadable passwd read as private")
	}
}

func TestUpgradedSameOwner0644LockIsNarrowedNotInaccessible(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "profile")
	os.Mkdir(h, 0o700)
	lock := filepath.Join(h, "gateway.lock")
	os.WriteFile(lock, nil, 0o644)
	os.Chmod(lock, 0o644)
	mode := func() os.FileMode { info, _ := os.Stat(lock); return info.Mode().Perm() }
	if got := MissingOwnerState(h); got != "absent" || mode() != 0o600 {
		t.Fatalf("%s %o", got, mode())
	}
	os.Chmod(lock, 0o644)
	owner := NewProfileOwnership()
	if err := owner.Reserve(context.Background(), []string{h}, ""); err != nil {
		t.Fatal(err)
	}
	if mode() != 0o600 || MissingOwnerState(h) != "starting" {
		owner.Close()
		t.Fatalf("%o", mode())
	}
	owner.Close()
	os.Chmod(lock, 0o644)
	os.Link(lock, filepath.Join(tmp, "alias"))
	if got := MissingOwnerState(h); got != "inaccessible" || mode() != 0o644 {
		t.Fatalf("%s %o", got, mode())
	}
}

func TestClassifyRefusedOwner(t *testing.T) {
	u := func(v uint32) *uint32 { return &v }
	b := func(v bool) *bool { return &v }
	for i, c := range []struct {
		own, owner    *uint32
		ownElevated   bool
		ownerElevated *bool
		want          string
	}{
		{u(1), u(0), false, nil, GatewayOtherSession},
		{u(1), u(0), true, b(true), GatewayOtherSession},
		{u(1), u(1), false, b(true), GatewayElevated},
		{u(1), u(1), false, nil, GatewayElevated},
		{u(1), u(1), false, b(false), ""},
		{u(1), u(1), true, nil, ""},
		{u(1), nil, false, nil, ""},
	} {
		if got := ClassifyRefusedOwner(c.own, c.owner, c.ownElevated, c.ownerElevated); got != c.want {
			t.Fatalf("case %d: %q", i, got)
		}
	}
}

// Live icebath gateway run lookalike, or a plain process, reaped when the test ends
func gatewayLookalike(t *testing.T, asGateway bool) int {
	tmp := t.TempDir()
	holder := spawnLockHolder(t, filepath.Join(tmp, "bin"), filepath.Join(tmp, "holder-home"), asGateway)
	t.Cleanup(holder.stop)
	return holder.pid
}

func refusalRecord(h string, pid int) *pyjson.Object {
	r := pyjson.NewObject()
	r.Set("pid", pid)
	r.Set("kind", gatewayKind)
	r.Set("argv", []any{"icebath", "gateway", "run"})
	r.Set("start_time", statusProcessStartTime(pid))
	r.Set("hermes_home", h)
	return r
}

func writeRefusalRecord(t *testing.T, path string, r *pyjson.Object) {
	body, err := pyjson.Dumps(r)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(body), 0o644)
}

func refusingOwner(t *testing.T, pid int, reason, remedy string, found bool) (string, *[]int) {
	h := filepath.Join(t.TempDir(), "home")
	os.Mkdir(h, 0o755)
	writeRefusalRecord(t, filepath.Join(h, "gateway.pid"), refusalRecord(h, pid))
	seen := &[]int{}
	patch(t, &probeRefusedOwnerSeam, func(p int) (string, string, bool, error) {
		*seen = append(*seen, p)
		return reason, remedy, found, nil
	})
	return h, seen
}

func TestRefusalNamesTheOtherSessionOwnerWithItsRemedy(t *testing.T) {
	pid := gatewayLookalike(t, true)
	zero := uint32(0)
	h, seen := refusingOwner(t, pid, GatewayOtherSession, RefusedOwnerRemedy(GatewayOtherSession, pid, &zero), true)
	result := refusedVerdict(h, true)
	if len(*seen) != 1 || (*seen)[0] != pid || result.State != "inaccessible" || result.ReasonCode != GatewayOtherSession {
		t.Fatalf("%v %+v", *seen, result)
	}
	if !strings.Contains(result.Detail, fmt.Sprintf("PID %d", pid)) || !strings.Contains(result.Detail, "session 0") {
		t.Fatal(result.Detail)
	}
	if code, err := EnsureExitCode(result); err != nil || code != 4 {
		t.Fatal(code, err)
	}
}

func TestRefusalNamesTheElevatedOwnerWithItsRemedy(t *testing.T) {
	pid := gatewayLookalike(t, true)
	one := uint32(1)
	h, _ := refusingOwner(t, pid, GatewayElevated, RefusedOwnerRemedy(GatewayElevated, pid, &one), true)
	result := refusedVerdict(h, true)
	if result.ReasonCode != GatewayElevated || !strings.Contains(result.Detail, "administrator") || !strings.Contains(result.Detail, "icebath gateway stop") {
		t.Fatalf("%+v", result)
	}
}

func TestUnnamedOrUnprobeableRefusalKeepsAuthorization(t *testing.T) {
	pid := gatewayLookalike(t, true)
	h, _ := refusingOwner(t, pid, "", "", false)
	if r := refusedVerdict(h, true); r.ReasonCode != "authorization" {
		t.Fatalf("%+v", r)
	}
	patch(t, &probeRefusedOwnerSeam, func(int) (string, string, bool, error) { return "", "", false, errors.New("x") })
	if r := refusedVerdict(h, true); r.ReasonCode != "authorization" {
		t.Fatalf("%+v", r)
	}
	os.Remove(filepath.Join(h, "gateway.pid"))
	if r := refusedVerdict(h, true); r.ReasonCode != "authorization" {
		t.Fatalf("%+v", r)
	}
}

func TestDiscoveryRoutesARefusedControlChannelThroughRefused(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	os.Mkdir(h, 0o755)
	var seen []string
	patch(t, &refusedSeam, func(home string) ObservedEndpoint {
		seen = append(seen, home)
		return ObservedEndpoint{State: "inaccessible", ReasonCode: GatewayElevated}
	})
	patch(t, &queryIdentifySeam, func(string, time.Duration) (*pyjson.Object, error) {
		return nil, fmt.Errorf("runtime control pipe inaccessible: %w", fs.ErrPermission)
	})
	result := discover(h, time.Second)
	resolved, _ := filepath.EvalSymlinks(h)
	if result.State != "inaccessible" || result.ReasonCode != GatewayElevated || len(seen) != 1 || seen[0] != resolved {
		t.Fatalf("%+v %v", result, seen)
	}
}

func TestOffWindowsARefusalStaysAuthorizationWithoutProbing(t *testing.T) {
	pid := gatewayLookalike(t, true)
	h, seen := refusingOwner(t, pid, GatewayElevated, "x", true)
	if r := refusedSeam(h); r.ReasonCode != "authorization" || len(*seen) != 0 {
		t.Fatalf("%+v %v", r, *seen)
	}
}

func probeRecorder(t *testing.T) *[]int {
	seen := &[]int{}
	patch(t, &probeRefusedOwnerSeam, func(pid int) (string, string, bool, error) {
		*seen = append(*seen, pid)
		one := uint32(1)
		return GatewayElevated, RefusedOwnerRemedy(GatewayElevated, pid, &one), true, nil
	})
	return seen
}

func TestARecycledPidIsNeverNamed(t *testing.T) {
	pid := gatewayLookalike(t, true)
	h := filepath.Join(t.TempDir(), "home")
	os.Mkdir(h, 0o755)
	record := refusalRecord(h, pid)
	start, _ := statusProcessStartTime(pid).(int64)
	record.Set("start_time", start+12345)
	writeRefusalRecord(t, filepath.Join(h, "gateway.pid"), record)
	seen := probeRecorder(t)
	if r := refusedVerdict(h, true); r.ReasonCode != "authorization" || len(*seen) != 0 {
		t.Fatalf("%+v %v", r, *seen)
	}
}

func TestAnUnverifiableOwnerIsNeverNamed(t *testing.T) {
	for _, shape := range []string{"no start time", "not a gateway", "another home"} {
		t.Run(shape, func(t *testing.T) {
			h := filepath.Join(t.TempDir(), "home")
			os.Mkdir(h, 0o755)
			pid := gatewayLookalike(t, shape != "not a gateway")
			record := refusalRecord(h, pid)
			switch shape {
			case "no start time":
				record.Delete("start_time")
			case "another home":
				record.Set("hermes_home", filepath.Join(filepath.Dir(h), "other-profile"))
			}
			writeRefusalRecord(t, filepath.Join(h, "gateway.pid"), record)
			seen := probeRecorder(t)
			if r := refusedVerdict(h, true); r.ReasonCode != "authorization" || len(*seen) != 0 {
				t.Fatalf("%+v %v", r, *seen)
			}
		})
	}
}

func TestAStalePidFileFallsThroughToTheLockOwner(t *testing.T) {
	pid := gatewayLookalike(t, true)
	h := filepath.Join(t.TempDir(), "home")
	os.Mkdir(h, 0o755)
	dead := exec.Command("true")
	dead.Run()
	stale := pyjson.NewObject()
	stale.Set("pid", dead.Process.Pid)
	stale.Set("start_time", 1)
	writeRefusalRecord(t, filepath.Join(h, "gateway.pid"), stale)
	writeRefusalRecord(t, filepath.Join(h, "gateway.lock"), refusalRecord(h, pid))
	seen := probeRecorder(t)
	if r := refusedVerdict(h, true); r.ReasonCode != GatewayElevated || len(*seen) != 1 || (*seen)[0] != pid {
		t.Fatalf("%+v %v", r, *seen)
	}
}
