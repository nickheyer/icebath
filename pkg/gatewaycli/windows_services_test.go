package gatewaycli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

type fakeSCMService struct {
	name, binpath, status string
	pid                   int
	nameErr, binpathErr   error
	statusErr             error
}

func (f fakeSCMService) Name() (string, error)    { return f.name, f.nameErr }
func (f fakeSCMService) BinPath() (string, error) { return f.binpath, f.binpathErr }
func (f fakeSCMService) Status() (string, error)  { return f.status, f.statusErr }
func (f fakeSCMService) PID() (int, error)        { return f.pid, f.statusErr }

// Runs the finder as on Windows over fake services and the tree 18480 under 12296 under 2360
func runWindowsFinder(t *testing.T, services ...scmService) ([]WindowsGatewayService, error) {
	t.Helper()
	savedOS, savedEnum, savedCreate, savedParents, savedKids, savedRoots := windowsServicesGOOS, scmEnumerate, winProcessCreateTime, winProcessParents, winProcessDescendants, windowsServiceRoots
	t.Cleanup(func() {
		windowsServicesGOOS, scmEnumerate, winProcessCreateTime, winProcessParents, winProcessDescendants, windowsServiceRoots = savedOS, savedEnum, savedCreate, savedParents, savedKids, savedRoots
	})
	windowsServicesGOOS = "windows"
	scmEnumerate = func() ([]scmService, error) { return services, nil }
	winProcessCreateTime = func(pid int) (float64, error) { return float64(pid), nil }
	winProcessParents = func(pid int) ([]int, error) { return []int{12296, 2360, 4}, nil }
	winProcessDescendants = func(pid int) ([]ProcessIdentity, error) {
		if pid != 2360 {
			t.Fatalf("descendants asked of pid %d", pid)
		}
		return []ProcessIdentity{{18480, 18480}, {12296, 12296}}, nil
	}
	windowsServiceRoots = func(context.Context) []string { return []string{`C:\icebath\icebath-agent`} }
	return findWindowsGatewayServices(context.Background(), []ProfileGatewayProcess{{Profile: "default", PID: 18480, CreateTime: 18480.0}}, true)
}

func summarize(services []WindowsGatewayService) []string {
	out := []string{}
	for _, s := range services {
		out = append(out, strings.Join([]string{s.Name, s.Profile}, "@"))
	}
	return out
}

func TestFindWindowsGatewayServicesIgnoresTaskSchedulerAncestor(t *testing.T) {
	running := func(name, binpath string) fakeSCMService {
		return fakeSCMService{name: name, binpath: binpath, status: "running", pid: 2360}
	}
	found, err := runWindowsFinder(t, running("Schedule", `C:\Windows\system32\svchost.exe -k netsvcs -p -s Schedule`))
	if err != nil || len(found) != 0 {
		t.Fatalf("task scheduler host read as a supervisor: %v %v", found, err)
	}
	owned, err := runWindowsFinder(t, running("gw", `"C:\icebath\icebath-agent\venv\Scripts\icebath.exe" gateway run`))
	if err != nil || len(owned) != 1 {
		t.Fatalf("owned service %v %v", owned, err)
	}
	want := WindowsGatewayService{Name: "gw", Profile: "default", ServicePID: 2360, GatewayPID: 18480, DescendantPIDs: []int{12296, 18480},
		DescendantIdentities: []ProcessIdentity{{12296, 12296}, {18480, 18480}}, ServiceCreateTime: 2360, GatewayCreateTime: 18480}
	if !reflect.DeepEqual(owned[0], want) {
		t.Fatalf("service %+v", owned[0])
	}
	unreadable := func(name string, err error) fakeSCMService {
		return fakeSCMService{name: name, status: "running", pid: 2360, binpathErr: err}
	}
	if found, err := runWindowsFinder(t, unreadable("Hardened", &scmQueryError{Class: "AccessDenied", Err: errors.New("denied")})); err != nil || len(found) != 0 {
		t.Fatalf("hardened service %v %v", found, err)
	}
	if found, err := runWindowsFinder(t, unreadable("BrokenMui", syscall.Errno(15100))); err != nil || len(found) != 0 {
		t.Fatalf("service with an unreadable config %v %v", found, err)
	}
	named, err := runWindowsFinder(t, unreadable("IcebathGateway", syscall.Errno(15100)))
	if err != nil || !reflect.DeepEqual(summarize(named), []string{"IcebathGateway@default"}) || named[0].ServicePID != 2360 || named[0].GatewayPID != 18480 {
		t.Fatalf("named service %v %v", named, err)
	}
}

func TestFindWindowsGatewayServicesFailsClosedOnAmbiguousOrUnreadableOwnership(t *testing.T) {
	message := func(err error) string {
		if err == nil {
			return ""
		}
		text := err.Error()
		for cause := errors.Unwrap(err); cause != nil; cause = errors.Unwrap(cause) {
			text += " <- " + cause.Error()
		}
		return text
	}
	svc := func(name, status string, pid int) fakeSCMService {
		return fakeSCMService{name: "icebath-" + name, status: status, pid: pid}
	}
	for _, c := range []struct {
		name     string
		services []scmService
		want     string
	}{
		{"indeterminate", []scmService{svc("a", "stop_pending", 2360)}, "SCM service icebath-a has indeterminate status: stop_pending"},
		{"shared", []scmService{svc("a", "running", 2360), svc("b", "running", 2360)}, "Gateway ownership is ambiguous under shared SCM host PID(s): 2360"},
		{"no pid", []scmService{svc("a", "running", 0)}, "SCM service enumeration failed <- Running SCM service icebath-a has no valid process ID"},
		{"empty name", []scmService{fakeSCMService{status: "running", pid: 2360}}, "SCM service enumeration failed <- SCM service inspection failed <- SCM service has an empty name"},
		{"vanished", []scmService{fakeSCMService{name: "icebath-a", statusErr: &scmQueryError{Class: "NoSuchProcess", Err: errors.New("gone")}}},
			"SCM service enumeration failed <- SCM service inspection failed <- NoSuchProcess: gone <- gone"},
	} {
		found, err := runWindowsFinder(t, c.services...)
		if got := message(err); got != c.want || found != nil {
			t.Errorf("%s: %q %v want %q", c.name, got, found, c.want)
		}
	}
	for _, c := range []struct {
		name     string
		services []scmService
	}{
		{"stopped", []scmService{svc("a", "stopped", 0)}},
		{"pending elsewhere", []scmService{svc("a", "start_pending", 999)}},
		{"deleted while inspected", []scmService{fakeSCMService{name: "icebath-a", statusErr: syscall.ENOENT}}},
		{"unowned descendant", []scmService{svc("a", "running", 4000)}},
	} {
		if found, err := runWindowsFinder(t, c.services...); err != nil || len(found) != 0 {
			t.Errorf("%s: %v %v", c.name, found, err)
		}
	}
	if _, err := runWindowsFinder(t, svc("a", "running", 2360)); err != nil {
		t.Fatal(err)
	}
	winProcessCreateTime = func(pid int) (float64, error) { return float64(pid) + 5, nil }
	_, err := findWindowsGatewayServices(context.Background(), []ProfileGatewayProcess{{Profile: "default", PID: 18480, CreateTime: 18480.0}}, true)
	if message(err) != "Gateway process identity changed during SCM discovery" {
		t.Fatalf("identity change: %q", message(err))
	}
	winProcessCreateTime = func(pid int) (float64, error) { return 0, errors.New("no such process") }
	_, err = findWindowsGatewayServices(context.Background(), []ProfileGatewayProcess{{Profile: "work", PID: 18480, CreateTime: 18480.0}}, true)
	if message(err) != "Could not determine SCM ownership for gateway profile work <- no such process" {
		t.Fatalf("probe failure: %q", message(err))
	}
	scmEnumerate = func() ([]scmService, error) { return nil, errors.New("rpc server unavailable") }
	if _, err = findWindowsGatewayServices(context.Background(), nil, true); message(err) != "SCM service enumeration failed <- rpc server unavailable" {
		t.Fatalf("enumeration failure: %q", message(err))
	}
	windowsServicesGOOS = "linux"
	scmEnumerate = func() ([]scmService, error) {
		t.Fatal("SCM consulted off Windows")
		return nil, nil
	}
	if found, err := FindWindowsGatewayServices(context.Background()); err != nil || found == nil || len(found) != 0 {
		t.Fatalf("off Windows %v %v", found, err)
	}
}
