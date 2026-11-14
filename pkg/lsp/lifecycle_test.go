package lsp

import "testing"

// Clears the service registry around a test
func resetRegistry(t *testing.T) {
	registryMu.Lock()
	prev := services
	services = map[string]*Service{}
	registryMu.Unlock()
	t.Cleanup(func() {
		ShutdownServices()
		registryMu.Lock()
		services = prev
		registryMu.Unlock()
	})
}

func TestServiceForBuildsOncePerHome(t *testing.T) {
	resetRegistry(t)
	rt, _, _ := testRuntime(t)
	builds := 0
	build := func() (*Service, error) {
		builds++
		return NewService(rt, Options{Enabled: true, WaitMode: "document", WaitTimeout: 1, InstallStrategy: "manual"}), nil
	}
	a, _ := ServiceFor("/h1", build)
	b, _ := ServiceFor("/h1", build)
	c, _ := ServiceFor("/h2", build)
	if a == nil || a != b || c == a || builds != 2 {
		t.Fatalf("a %p b %p c %p builds %d", a, b, c, builds)
	}
	disabled, _ := ServiceFor("/h3", func() (*Service, error) { return NewService(rt, Options{}), nil })
	again, _ := ServiceFor("/h3", func() (*Service, error) { t.Fatal("rebuilt a cached disabled service"); return nil, nil })
	if disabled != nil || again != nil {
		t.Fatal("a disabled service is never handed out")
	}
}

func TestAtExitShutdownSwallowsFailures(t *testing.T) {
	prev := shutdownServicesHook
	shutdownServicesHook = func() { panic("server already dead") }
	defer func() { shutdownServicesHook = prev }()
	AtExitShutdown()
}

func TestShutdownServicesIdempotent(t *testing.T) {
	resetRegistry(t)
	rt, _, _ := testRuntime(t)
	svc, _ := ServiceFor("/h", func() (*Service, error) {
		return NewService(rt, Options{Enabled: true, WaitMode: "document", WaitTimeout: 1, InstallStrategy: "manual", IdleTimeout: 60}), nil
	})
	ShutdownServices()
	ShutdownServices()
	if svc.l.running() {
		t.Fatal("loop still running after shutdown")
	}
	registryMu.Lock()
	n := len(services)
	registryMu.Unlock()
	if n != 0 {
		t.Fatalf("registry kept %d services", n)
	}
}
