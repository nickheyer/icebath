//go:build !windows

package tools

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

func TestInstallStateGatewayServiceCountsOnlyForItsAccount(t *testing.T) {
	isolateServiceTables(t)
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	theirs := "User=nobody\n"
	if me.Username == "nobody" {
		theirs = "User=root\n"
	}
	for name, identity := range map[string]string{"mine": "User=" + me.Username + "\n", "theirs": theirs, "root": ""} {
		installWriteFile(t, filepath.Join(tmp, "hermes-gateway-"+name+".service"),
			"[Service]\n"+identity+"Environment=\"HERMES_HOME="+filepath.Join(tmp, name)+"\"\n")
	}
	swapForTest(t, &gatewayServiceDefinitionGlobs, func() []string { return []string{filepath.Join(tmp, "hermes-gateway-[mt]*.service")} })
	swapForTest(t, &systemGatewayServiceDefinitionGlobs, func() []string { return []string{filepath.Join(tmp, "hermes-gateway-root.service")} })

	homes := collectServiceHomes()
	if !homes[filepath.Join(tmp, "mine")] {
		t.Fatalf("this account's unit is missing from %v", homes)
	}
	if homes[filepath.Join(tmp, "theirs")] {
		t.Fatalf("another account's unit counted: %v", homes)
	}
	if homes[filepath.Join(tmp, "root")] != (me.Username == "root") {
		t.Fatalf("machine wide unit with no User line counted wrongly for %q: %v", me.Username, homes)
	}
}

func TestLocateCommandRecordsEveryCandidate(t *testing.T) {
	tmp := t.TempDir()
	bin, known := filepath.Join(tmp, "bin"), filepath.Join(tmp, "known")
	installWriteFile(t, filepath.Join(known, "tool"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(known, "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	empty := ""
	if r := locateCommand("tool", &commandLookupContext{Path: &empty}, []string{known}); r.Kind != "known_path" ||
		r.Source() != "known_dir:"+known || len(r.Candidates) != 2 || r.Candidates[0].Present {
		t.Fatalf("resolution %+v", r)
	}
	t.Setenv("PATH", known)
	if r := locateCommand(" tool ", nil, nil); r.Kind != "path_executable" || r.Command()[0] != filepath.Join(known, "tool") {
		t.Fatalf("resolution %+v", r)
	}
	if r := locateCommand(filepath.Join(known, "tool"), nil, nil); r.Kind != "explicit_path" || !r.Found() {
		t.Fatalf("resolution %+v", r)
	}
	if r := locateCommand("missing", nil, []string{bin}); r.Found() || r.Source() != "none" || r.Command() != nil {
		t.Fatalf("resolution %+v", r)
	}
}
