package gateway

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	pipeOwnSID     = "S-1-5-21-1-2-3-1001"
	pipeOtherSID   = "S-1-5-21-1-2-3-1002"
	pipeEveryone   = "S-1-1-0"
	pipeServerPID  = 5_000_000
	pipeOwnPID     = 4242
	pipeTestHandle = uintptr(77)
)

// One pipe server's facts as the Win32 layer reports them to this client
type fakePipeFacts struct {
	t              *testing.T
	processSIDOf   *string
	owner          string
	aces           []pipeACE
	unreadable     bool
	ownElevated    bool
	ownSession     *uint32
	serverSession  *uint32
	clientPID      uint32
	serverElevated *bool
	clientSID      func(uintptr) (string, error)
	securityRead   func()
}

func u32(v uint32) *uint32 { return &v }
func bptr(v bool) *bool    { return &v }

func newFakePipe(t *testing.T, processSID *string) *fakePipeFacts {
	return &fakePipeFacts{t: t, processSIDOf: processSID, owner: pipeOwnSID, aces: []pipeACE{{0, pipeOwnSID}},
		ownSession: u32(1), serverSession: u32(1)}
}

func (f *fakePipeFacts) pipeClientSID(uintptr) (string, error) {
	if f.clientSID != nil {
		return f.clientSID(pipeTestHandle)
	}
	return "", pipePermissionError("cannot impersonate named-pipe peer")
}
func (f *fakePipeFacts) namedPipeProcessID(_ uintptr, server bool) (uint32, bool) {
	if server {
		return pipeServerPID, true
	}
	return f.clientPID, f.clientPID != 0
}
func (f *fakePipeFacts) processSID(pid uint32) (string, error) {
	if pid != pipeServerPID && pid != f.clientPID {
		return pipeOwnSID, nil
	}
	if f.processSIDOf == nil {
		return "", pipePermissionError("cannot verify bootstrap peer process")
	}
	return *f.processSIDOf, nil
}
func (f *fakePipeFacts) pipeSecurity(h uintptr) (*pipeSecurity, error) {
	if f.securityRead != nil {
		f.securityRead()
	}
	if h != pipeTestHandle {
		f.t.Fatalf("handle %d", h)
	}
	if f.unreadable {
		return nil, pipePermissionError("cannot read named-pipe owner")
	}
	return &pipeSecurity{Owner: f.owner, ACEs: f.aces}, nil
}
func (f *fakePipeFacts) pipeServerSession(uintptr) *uint32 { return f.serverSession }
func (f *fakePipeFacts) sessionOf(uint32) *uint32          { return f.ownSession }
func (f *fakePipeFacts) processElevation(pid uint32) *bool {
	if pid != pipeServerPID || f.processSIDOf == nil {
		f.t.Fatalf("elevation probed for an unreadable server")
	}
	return f.serverElevated
}
func (f *fakePipeFacts) processIsElevated() bool { return f.ownElevated }
func (f *fakePipeFacts) ownPID() uint32          { return pipeOwnPID }

func pipeRefusal(t *testing.T, f *fakePipeFacts) error {
	t.Helper()
	subject, err := peerSubject(f, pipeTestHandle, true)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("attached as %q, err %v", subject, err)
	}
	return err
}

func reasonOf(err error) string {
	var refused *RefusedOwner
	if errors.As(err, &refused) {
		return refused.ReasonCode
	}
	return ""
}

func sp(s string) *string { return &s }

func TestAFilteredClientNamesAnElevatedGatewayFromItsPipe(t *testing.T) {
	for _, processSID := range []*string{nil, sp(pipeOwnSID)} {
		f := newFakePipe(t, processSID)
		f.owner = administratorsSID
		err := pipeRefusal(t, f)
		var refused *RefusedOwner
		if !errors.As(err, &refused) || refused.ReasonCode != "gateway_elevated" || refused.Pid != pipeServerPID {
			t.Fatalf("%v", err)
		}
		if err.Error() != RefusedOwnerRemedy("gateway_elevated", pipeServerPID, u32(1)) {
			t.Fatalf("%q", err)
		}
	}
}

func TestAnUnopenableSameUserPipeIsTheElevationBoundary(t *testing.T) {
	if r := reasonOf(pipeRefusal(t, newFakePipe(t, nil))); r != "gateway_elevated" {
		t.Fatalf("%q", r)
	}
}

func TestAReadableElevatedServerIsNamedWhateverOwnsItsPipe(t *testing.T) {
	f := newFakePipe(t, sp(pipeOwnSID))
	f.serverElevated = bptr(true)
	err := pipeRefusal(t, f)
	if reasonOf(err) != "gateway_elevated" || err.Error() != RefusedOwnerRemedy("gateway_elevated", pipeServerPID, u32(1)) {
		t.Fatalf("%v", err)
	}
}

func TestAFailingElevationProbeFallsBackToThePipeOwner(t *testing.T) {
	for _, c := range []struct {
		owner    string
		attached bool
	}{{administratorsSID, false}, {pipeOwnSID, true}} {
		f := newFakePipe(t, sp(pipeOwnSID))
		f.owner = c.owner
		if c.attached {
			if got, err := peerSubject(f, pipeTestHandle, true); err != nil || got != "sid:"+pipeOwnSID {
				t.Fatalf("%q %v", got, err)
			}
		} else if r := reasonOf(pipeRefusal(t, f)); r != "gateway_elevated" {
			t.Fatalf("%q", r)
		}
	}
}

func TestAnUnopenableGatewayInAnotherSessionIsNamedAsSuch(t *testing.T) {
	f := newFakePipe(t, nil)
	f.owner, f.serverSession = administratorsSID, u32(0)
	err := pipeRefusal(t, f)
	if reasonOf(err) != "gateway_other_session" || err.Error() != RefusedOwnerRemedy("gateway_other_session", pipeServerPID, u32(0)) {
		t.Fatalf("%v", err)
	}
}

func TestDiscoveryReportsTheNamedRefusalWithoutAnyOwnerRecord(t *testing.T) {
	f := newFakePipe(t, nil)
	f.owner = administratorsSID
	h := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(h, 0o700); err != nil {
		t.Fatal(err)
	}
	patch(t, &queryIdentifySeam, func(string, time.Duration) (*pyjson.Object, error) {
		_, err := peerSubject(f, pipeTestHandle, true)
		return nil, err
	})
	result := DiscoverGatewayEndpoint(context.Background(), h, time.Second)
	if result.State != "inaccessible" || result.ReasonCode != "gateway_elevated" ||
		result.Detail != RefusedOwnerRemedy("gateway_elevated", pipeServerPID, u32(1)) {
		t.Fatalf("%+v", result)
	}
	if code, err := EnsureExitCode(result); err != nil || code != 4 {
		t.Fatalf("exit %d %v", code, err)
	}
}

// A named profile under root that the default multiplexer serves, with the root query raising refusal
func servedSecondary(t *testing.T, refusal func() error) string {
	root := filepath.Join(t.TempDir(), "root")
	h := filepath.Join(root, "profiles", "work")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	patch(t, &queryIdentifySeam, func(target string, _ time.Duration) (*pyjson.Object, error) {
		if target != root {
			return nil, &nativePipeError{target, fs.ErrNotExist}
		}
		return nil, refusal()
	})
	return h
}

func TestAServedSecondaryDiscoversItsElevatedMultiplexerByName(t *testing.T) {
	f := newFakePipe(t, nil)
	f.owner = administratorsSID
	h := servedSecondary(t, func() error { _, err := peerSubject(f, pipeTestHandle, true); return err })
	found := DiscoverGatewayEndpoint(context.Background(), h, time.Second)
	if found.State != "inaccessible" || found.ReasonCode != "gateway_elevated" ||
		found.Detail != RefusedOwnerRemedy("gateway_elevated", pipeServerPID, u32(1)) {
		t.Fatalf("%+v", found)
	}
}

func TestAServedSecondaryKeepsIgnoringAnyOtherMultiplexerRefusal(t *testing.T) {
	h := servedSecondary(t, func() error { return pipePermissionError("no token") })
	if got := DiscoverGatewayEndpoint(context.Background(), h, time.Second); got.State != "absent" {
		t.Fatalf("%+v", got)
	}
}

func TestAGenericMultiplexerRefusalNeverReadsTheProfileConfigs(t *testing.T) {
	h := servedSecondary(t, func() error { return pipePermissionError("no token") })
	bad := "gateway: [unclosed\n  multiplex_profiles: {\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(h)), "config.yaml"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverGatewayEndpoint(context.Background(), h, time.Second); got.State != "absent" {
		t.Fatalf("%+v", got)
	}
}

func TestAnUnprovablePipeServerStaysTheGenericRefusal(t *testing.T) {
	cases := []struct {
		name     string
		owner    string
		aces     []pipeACE
		readable bool
	}{
		{"other-owner", pipeOtherSID, []pipeACE{{0, pipeOtherSID}}, true},
		{"other-owner-own-dacl", pipeOtherSID, []pipeACE{{0, pipeOwnSID}}, true},
		{"admins-everyone", administratorsSID, []pipeACE{{0, pipeEveryone}}, true},
		{"admins-extra-ace", administratorsSID, []pipeACE{{0, pipeOwnSID}, {0, pipeEveryone}}, true},
		{"deny-ace", administratorsSID, []pipeACE{{1, pipeOwnSID}, {0, pipeOwnSID}}, true},
		{"null-dacl", administratorsSID, nil, true},
		{"own-owner-other-dacl", pipeOwnSID, []pipeACE{{0, pipeOtherSID}}, true},
		{"unreadable", "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakePipe(t, nil)
			f.owner, f.aces, f.unreadable = c.owner, c.aces, !c.readable
			err := pipeRefusal(t, f)
			if reasonOf(err) != "" || !(strings.Contains(err.Error(), "cannot verify") || strings.Contains(err.Error(), "another user")) {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestAnElevatedClientNeverAttachesToAServerItCannotOpen(t *testing.T) {
	f := newFakePipe(t, nil)
	f.owner, f.ownElevated = administratorsSID, true
	if err := pipeRefusal(t, f); reasonOf(err) != "" || err.Error() != "cannot verify bootstrap peer process" {
		t.Fatalf("%v", err)
	}
}

func TestAnotherUsersReadableServerIsRefused(t *testing.T) {
	f := newFakePipe(t, sp(pipeOtherSID))
	f.owner, f.aces = pipeOtherSID, []pipeACE{{0, pipeOtherSID}}
	if err := pipeRefusal(t, f); err.Error() != "named-pipe peer belongs to another user" {
		t.Fatalf("%v", err)
	}
}

func TestASameUserServerAtTheClientsLevelIsAttached(t *testing.T) {
	for _, c := range []struct {
		owner              string
		ownElevated, reads bool
	}{{pipeOwnSID, false, true}, {administratorsSID, true, true}, {"", false, false}} {
		f := newFakePipe(t, sp(pipeOwnSID))
		f.owner, f.ownElevated, f.unreadable = c.owner, c.ownElevated, !c.reads
		if got, err := peerSubject(f, pipeTestHandle, true); err != nil || got != "sid:"+pipeOwnSID {
			t.Fatalf("%+v: %q %v", c, got, err)
		}
	}
}

func TestAReadableServerAtTheClientsElevationIsAttached(t *testing.T) {
	for _, c := range []struct {
		owner                       string
		ownElevated, serverElevated bool
	}{{pipeOwnSID, true, true}, {administratorsSID, true, true}, {pipeOwnSID, false, false}, {administratorsSID, false, false}} {
		f := newFakePipe(t, sp(pipeOwnSID))
		f.owner, f.ownElevated, f.serverElevated = c.owner, c.ownElevated, bptr(c.serverElevated)
		if got, err := peerSubject(f, pipeTestHandle, true); err != nil || got != "sid:"+pipeOwnSID {
			t.Fatalf("%+v: %q %v", c, got, err)
		}
	}
}

func TestTheServingSideNeverJudgesAClientByThePipeOwner(t *testing.T) {
	f := newFakePipe(t, nil)
	f.clientPID = pipeServerPID + 1
	f.clientSID = func(uintptr) (string, error) { return "", pipePermissionError("no token") }
	f.securityRead = func() { t.Fatalf("pipe owner read for a client") }
	if _, err := peerSubject(f, pipeTestHandle, false); err == nil || !strings.Contains(err.Error(), "cannot verify bootstrap peer process") {
		t.Fatalf("%v", err)
	}
}

func TestServingSideIdentifiesAPeerByItsCapturedTokenAndRaisesAnImpersonationLeak(t *testing.T) {
	f := newFakePipe(t, nil)
	f.clientSID = func(uintptr) (string, error) { return pipeOwnSID, nil }
	if got, err := peerSubject(f, pipeTestHandle, false); err != nil || got != "sid:"+pipeOwnSID {
		t.Fatalf("%q %v", got, err)
	}
	f.clientSID = func(uintptr) (string, error) { return "", impersonationLeakError{} }
	if _, err := peerSubject(f, pipeTestHandle, false); !errors.As(err, new(impersonationLeakError)) {
		t.Fatalf("%v", err)
	}
}

func TestWindowsPipeQueryIsBoundedWhenThePeerNeverAnswers(t *testing.T) {
	released := make(chan struct{})
	defer close(released)
	patch(t, &windowsPipeExchangeSeam, func(string, []byte, time.Duration) ([]byte, error) {
		select {
		case <-released:
		case <-time.After(10 * time.Second):
		}
		return []byte{}, nil
	})
	start := time.Now()
	got, err := queryWindowsPipe(t.TempDir(), []byte("{}\n"), 300*time.Millisecond)
	if got != nil || err != nil || time.Since(start) >= 2*time.Second {
		t.Fatalf("%q %v %s", got, err, time.Since(start))
	}
}

func TestRunBoundedSyncReturnsValueReraisesErrorAndAbandonsAtTheDeadline(t *testing.T) {
	value, timedOut := runBoundedSync(func() (int, error) { return 7, nil }, time.Second, "op")
	if v, err := value(); timedOut || v != 7 || err != nil {
		t.Fatalf("%d %v %v", v, err, timedOut)
	}
	boom := errors.New("boom")
	value, _ = runBoundedSync(func() (int, error) { return 0, boom }, time.Second, "op")
	if _, err := value(); err != boom {
		t.Fatalf("%v", err)
	}
	value, _ = runBoundedSync(func() (int, error) { return 3, nil }, 0, "op")
	if v, _ := value(); v != 3 {
		t.Fatalf("unbounded %d", v)
	}
	release := make(chan struct{})
	defer close(release)
	if _, timedOut := runBoundedSync(func() (int, error) { <-release; return 1, nil }, 50*time.Millisecond, "op"); !timedOut {
		t.Fatal("not abandoned")
	}
}

func TestQueryRuntimeControlRaisesOffWindows(t *testing.T) {
	if _, err := queryRuntimeControl(t.TempDir(), []byte("{}"), time.Second); err == nil ||
		err.Error() != "native Windows bootstrap requires Windows" {
		t.Fatalf("%v", err)
	}
	server := NewNativeControlServer(t.TempDir(), func([]byte, string) []byte { return nil })
	if err := server.Start(); err == nil || err.Error() != "native Windows bootstrap requires Windows" {
		t.Fatalf("%v", err)
	}
	if err := server.Close(); err != nil || server.Alive(0) {
		t.Fatalf("%v", err)
	}
}

// In memory pipe instance fed one scripted client per accept
type fakeNativePipe struct {
	mu          sync.Mutex
	clients     chan []byte
	current     []byte
	written     [][]byte
	disconnects int
	subject     func() (string, error)
	closed      bool
}

func (p *fakeNativePipe) accept(budget time.Duration) (bool, error) {
	select {
	case c := <-p.clients:
		p.mu.Lock()
		p.current = c
		p.mu.Unlock()
		return true, nil
	case <-time.After(budget):
		return false, nil
	}
}
func (p *fakeNativePipe) readLine(time.Time, int) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return nil, &nativePipeError{"runtime pipe closed before response", errPipeConnection}
	}
	line, _, _ := bytes.Cut(p.current, []byte{'\n'})
	p.current = nil
	return line, nil
}
func (p *fakeNativePipe) write(data []byte, _ time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.written = append(p.written, data)
	return nil
}
func (p *fakeNativePipe) peerSubject() (string, error) {
	p.mu.Lock()
	subject := p.subject
	p.mu.Unlock()
	return subject()
}
func (p *fakeNativePipe) disconnect() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.disconnects++
	return nil
}
func (p *fakeNativePipe) close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
}

func TestNativeControlServerServesIdentifiedPeersAndStopsOnAnImpersonationLeak(t *testing.T) {
	identified := func() (string, error) { return "sid:" + pipeOwnSID, nil }
	first := &fakeNativePipe{clients: make(chan []byte, 4), subject: identified}
	var mu sync.Mutex
	var created []bool
	patch(t, &createNativePipeSeam, func(_ string, isFirst bool) (nativePipeInstance, error) {
		mu.Lock()
		defer mu.Unlock()
		created = append(created, isFirst)
		if isFirst {
			return first, nil
		}
		return &fakeNativePipe{clients: make(chan []byte), subject: identified}, nil
	})
	var subjects []string
	server := NewNativeControlServer(t.TempDir(), func(raw []byte, subject string) []byte {
		mu.Lock()
		subjects = append(subjects, subject+"|"+string(raw))
		mu.Unlock()
		return []byte("{\"ok\": true}\n")
	})
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	first.clients <- []byte("hello\n")
	deadline := time.Now().Add(5 * time.Second)
	for {
		first.mu.Lock()
		n := len(first.written)
		first.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	first.mu.Lock()
	first.subject = func() (string, error) { return "", impersonationLeakError{} }
	first.mu.Unlock()
	first.clients <- []byte("again\n")
	for server.Alive(0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(created) != nativePipeInstances || !created[0] || created[1] {
		t.Fatalf("created %v", created)
	}
	if len(subjects) != 1 || subjects[0] != "sid:"+pipeOwnSID+"|hello" {
		t.Fatalf("subjects %v", subjects)
	}
	if !errors.As(server.Err(), new(impersonationLeakError)) || !first.closed || first.disconnects != 2 {
		t.Fatalf("err %v closed %v disconnects %d", server.Err(), first.closed, first.disconnects)
	}
}
