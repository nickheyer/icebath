package main

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestSetProcessTitleNamesTheCallingThreadCappedAtFifteenBytes(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	read := func() string {
		b, err := os.ReadFile("/proc/thread-self/comm")
		if err != nil {
			t.Skip("no /proc/thread-self/comm: " + err.Error())
		}
		return strings.TrimRight(string(b), "\n")
	}
	before := read()
	defer setProcessTitleOS(before)
	setProcessTitleOS("icebath")
	if got := read(); got != "icebath" {
		t.Fatalf("thread name %q", got)
	}
	setProcessTitleOS("icebath-with-a-long-title")
	if got := read(); got != "icebath-with-a-" {
		t.Fatalf("long title named the thread %q", got)
	}
	setProcessTitle()
	if got := read(); got != "icebath" {
		t.Fatalf("setProcessTitle named the thread %q", got)
	}
}
