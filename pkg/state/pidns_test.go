package state

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// Pins the local PID namespace resolution for one test
func pinPidNamespace(t *testing.T, cached *LocalPidNamespace, resolve func() LocalPidNamespace) {
	localPidNSMu.Lock()
	prevCached, prevResolve := localPidNS, resolveLocalPidNamespace
	localPidNS, resolveLocalPidNamespace = cached, resolve
	localPidNSMu.Unlock()
	t.Cleanup(func() {
		localPidNSMu.Lock()
		localPidNS, resolveLocalPidNamespace = prevCached, prevResolve
		localPidNSMu.Unlock()
	})
}

func TestHolderPidNamespaceMatrix(t *testing.T) {
	pinPidNamespace(t, &LocalPidNamespace{ID: "111", Supported: true}, nil)
	if HolderNamespaceToken() != ":pidns=111" {
		t.Errorf("token = %q", HolderNamespaceToken())
	}
	cases := map[string]bool{
		"pid=1:pidns=111:turn=a": true,
		"pid=1:pidns=222:turn=a": false,
		"pid=1:turn=a":           false,
		"pidns=111":              true,
		"pid=1:xpidns=111":       false,
		"":                       false,
	}
	for holder, want := range cases {
		if got := HolderPidCheckable(holder); got != want {
			t.Errorf("HolderPidCheckable(%q) = %v", holder, got)
		}
	}
	if !PersistentRecordPidnsCheckable("", false) || PersistentRecordPidnsCheckable("222", true) || !PersistentRecordPidnsCheckable("111", true) {
		t.Error("persistent records probe unstamped and own namespace only")
	}
}

func TestFailedPidNamespaceLookupIsNotCached(t *testing.T) {
	pinPidNamespace(t, nil, func() LocalPidNamespace { return LocalPidNamespace{Supported: true} })
	if PidNamespaceID() != "" || HolderNamespaceToken() != "" {
		t.Error("a failed lookup asserts no namespace")
	}
	if PersistentRecordPidnsCheckable("111", true) || !PersistentRecordPidnsCheckable("", false) || HolderPidCheckable("pid=1") {
		t.Error("a failed lookup makes stamped records unverifiable")
	}
	localPidNSMu.Lock()
	resolveLocalPidNamespace = func() LocalPidNamespace { return LocalPidNamespace{ID: "111", Supported: true} }
	localPidNSMu.Unlock()
	if !PersistentRecordPidnsCheckable("111", true) {
		t.Error("the resolved namespace must be picked up once the lookup succeeds")
	}
}

func TestUnsupportedPlatformProbesEverything(t *testing.T) {
	pinPidNamespace(t, &LocalPidNamespace{}, nil)
	if !HolderPidCheckable("pid=1:pidns=222") || !HolderPidCheckable("pid=1") || HolderNamespaceToken() != "" {
		t.Error("a single namespace platform treats a pid as evidence")
	}
}

func TestResolvedLocalNamespaceMatchesProc(t *testing.T) {
	local := resolveLocalPidNamespace()
	if runtime.GOOS != "linux" {
		if local != (LocalPidNamespace{}) {
			t.Errorf("non linux resolution = %+v", local)
		}
		return
	}
	link, err := os.Readlink("/proc/self/ns/pid")
	want := LocalPidNamespace{Supported: true}
	if err == nil {
		want.ID = strings.TrimSuffix(strings.TrimPrefix(link, "pid:["), "]")
	}
	if local != want {
		t.Errorf("resolved %+v, want %+v", local, want)
	}
}
