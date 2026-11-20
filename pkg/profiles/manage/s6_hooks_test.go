package manage

import (
	"errors"
	"testing"
)

// Host backend whose registration calls fail the test because the hooks must never reach them
type hostServiceManager struct{ t *testing.T }

func (hostServiceManager) SupportsRuntimeRegistration() bool { return false }
func (m hostServiceManager) RegisterProfileGateway(string, bool) error {
	m.t.Error("host backend register_profile_gateway should not be called")
	return nil
}
func (m hostServiceManager) UnregisterProfileGateway(string) error {
	m.t.Error("host backend unregister_profile_gateway should not be called")
	return nil
}

// s6 manager recording registrations and the start_now it was handed
type fakeS6Manager struct {
	registered, unregistered []string
	lastStartNow             *bool
}

func (*fakeS6Manager) SupportsRuntimeRegistration() bool { return true }
func (m *fakeS6Manager) RegisterProfileGateway(profile string, startNow bool) error {
	m.registered = append(m.registered, profile)
	m.lastStartNow = &startNow
	return nil
}
func (m *fakeS6Manager) UnregisterProfileGateway(profile string) error {
	m.unregistered = append(m.unregistered, profile)
	return nil
}

// Restores the service manager seams after a case swaps them
func swapServiceManager(t *testing.T, detect func() (string, error), get func() (s6Manager, error)) {
	t.Helper()
	oldDetect, oldGet := detectServiceManager, getServiceManager
	if detect != nil {
		detectServiceManager = detect
	}
	getServiceManager = get
	t.Cleanup(func() { detectServiceManager, getServiceManager = oldDetect, oldGet })
}

func TestRegisterNoopOnHost(t *testing.T) {
	out := captureOut(t)
	swapServiceManager(t, nil, func() (s6Manager, error) { return hostServiceManager{t}, nil })
	maybeRegisterGatewayService("hostprof")
	if out.String() != "" {
		t.Fatalf("output %q", out.String())
	}
}

func TestRegisterPassesStartNowFalse(t *testing.T) {
	mgr := &fakeS6Manager{}
	swapServiceManager(t, func() (string, error) { return "s6", nil }, func() (s6Manager, error) { return mgr, nil })
	maybeRegisterGatewayService("coder")
	if mgr.lastStartNow == nil || *mgr.lastStartNow {
		t.Fatal("profile creation must not auto-start the gateway service")
	}
	if len(mgr.registered) != 1 || mgr.registered[0] != "coder" {
		t.Fatalf("registered %v", mgr.registered)
	}
}

func TestRegisterSilentWhenDetectThrows(t *testing.T) {
	out := captureOut(t)
	swapServiceManager(t, func() (string, error) { return "", errors.New("detection blew up") },
		func() (s6Manager, error) { return hostServiceManager{t}, nil })
	maybeRegisterGatewayService("anywhere")
	if out.String() != "" {
		t.Fatalf("output %q", out.String())
	}
}
