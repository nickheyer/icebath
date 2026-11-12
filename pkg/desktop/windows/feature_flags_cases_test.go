package windows

import "testing"

func TestWithoutLocalLocalModelsFollowThePlatformOnEveryChannel(t *testing.T) {
	for _, p := range []struct {
		goos, goarch string
		ships        bool
	}{{"windows", "amd64", true}, {"darwin", "arm64", true}, {"linux", "amd64", true}, {"linux", "arm64", true}, {"linux", "riscv64", false}, {"freebsd", "amd64", false}} {
		for _, argv := range [][]string{{}, {"Icebath.exe"}} {
			if got := ResolveFeatureFlags(p.goos, p.goarch, argv, true); got.LocalModels != p.ships {
				t.Fatalf("%+v %v", p, argv)
			}
		}
	}
}

func TestLocalInArgvOptsIntoLocalModelsOnAnyChannelAndPlatform(t *testing.T) {
	if !ResolveFeatureFlags("linux", "riscv64", []string{"Icebath.exe", "--local"}, true).LocalModels || !ResolveFeatureFlags("freebsd", "386", []string{"--local"}, true).LocalModels {
		t.Fatal("--local ignored")
	}
}

func TestIsCanaryTagRecognizesCanaryStampsAndRejectsStableDevTags(t *testing.T) {
	if !IsCanaryTag("v0.28.0+canary.20260818T123456Z") {
		t.Fatal("canary")
	}
	for _, tag := range []string{"v0.28.0-canary.20260818", "v0.28.0+canary.20260818123456", "v0.28.0", ""} {
		if IsCanaryTag(tag) {
			t.Fatal(tag)
		}
	}
}

func TestAssumeFreshInstallOnlyForUnpackagedFreshInstallRuns(t *testing.T) {
	if !ResolveFeatureFlags("linux", "amd64", []string{"--fresh-install"}, false).AssumeFreshInstall || ResolveFeatureFlags("linux", "amd64", []string{"--fresh-install"}, true).AssumeFreshInstall {
		t.Fatal("fresh install gate")
	}
}
