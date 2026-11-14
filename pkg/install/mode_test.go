package install

import "testing"

func TestBareArgsAreInstall(t *testing.T) {
	if ModeFromArgs(nil) != ModeInstall || ModeFromArgs([]string{"--foo", "bar"}) != ModeInstall {
		t.Fatal("bare args must select install")
	}
}

func TestUpdateFlagSelectsUpdate(t *testing.T) {
	if ModeFromArgs([]string{"--update"}) != ModeUpdate || ModeFromArgs([]string{"--something", "--update", "--else"}) != ModeUpdate {
		t.Fatal("--update must select update")
	}
}

func TestReinstallAndRepairFlagsForceSetup(t *testing.T) {
	for _, args := range [][]string{{"--reinstall"}, {"--repair"}, {"--foo", "--repair", "--bar"}} {
		if !ForceSetupFromArgs(args) {
			t.Fatalf("%v must force setup", args)
		}
	}
}

func TestBareOrUnrelatedArgsDoNotForceSetup(t *testing.T) {
	for _, args := range [][]string{nil, {"--foo", "bar"}, {"--update"}} {
		if ForceSetupFromArgs(args) {
			t.Fatalf("%v must not force setup", args)
		}
	}
}

func TestBareMacosInstallHandsOffBeforeAppkit(t *testing.T) {
	if !HandoffBeforeAppKit(true, ModeInstall, false) {
		t.Fatal("bare macOS install must hand off")
	}
}

func TestRepairUpdateAndOtherPlatformsBuildInstallerUI(t *testing.T) {
	cases := []struct {
		mac   bool
		mode  AppMode
		force bool
	}{{true, ModeInstall, true}, {true, ModeUpdate, false}, {true, ModeUpdate, true}, {false, ModeInstall, false}, {false, ModeInstall, true}, {false, ModeUpdate, false}}
	for _, c := range cases {
		if HandoffBeforeAppKit(c.mac, c.mode, c.force) {
			t.Fatalf("%+v must build the installer UI", c)
		}
	}
}

func TestForceSetupFlagsDoNotAffectModeSelection(t *testing.T) {
	if ModeFromArgs([]string{"--reinstall"}) != ModeInstall || ModeFromArgs([]string{"--repair"}) != ModeInstall {
		t.Fatal("repair flags must not flip to update")
	}
	if ModeFromArgs([]string{"--update", "--reinstall"}) != ModeUpdate {
		t.Fatal("repair flags must not flip to install")
	}
}
