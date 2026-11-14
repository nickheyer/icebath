package pm

import (
	"slices"
	"testing"
)

// Program Files root the Windows candidate tests pass
const catProgramFiles = `D:\Progs`

func TestCatSystemLauncherBashIsNeverACandidate(t *testing.T) {
	for _, launcher := range []string{`C:\Windows\System32\bash.exe`, `C:\WINDOWS\system32\bash.exe`,
		`C:\Users\u\AppData\Local\Microsoft\WindowsApps\bash.exe`} {
		if slices.Contains(WindowsBashCandidates(launcher, map[string]string{"ProgramFiles": catProgramFiles}), launcher) {
			t.Fatalf("%s is a candidate", launcher)
		}
	}
}

func TestCatProgramFilesGitPrecedesThePathBash(t *testing.T) {
	onPath := `C:\msys64\usr\bin\bash.exe`
	candidates := WindowsBashCandidates(onPath, map[string]string{"ProgramFiles": catProgramFiles})
	if candidates[0] != catProgramFiles+`\Git\bin\bash.exe` || candidates[len(candidates)-1] != onPath {
		t.Fatalf("candidates = %q", candidates)
	}
}

func TestCatPerUserAnd32bitGitRootsAreCandidates(t *testing.T) {
	candidates := WindowsBashCandidates("", map[string]string{
		"ProgramFiles": catProgramFiles, "ProgramFiles(x86)": `D:\Progs32`, "LOCALAPPDATA": `C:\Users\u\AppData\Local`,
	})
	for _, want := range []string{`D:\Progs32\Git\bin\bash.exe`, `C:\Users\u\AppData\Local\Programs\Git\bin\bash.exe`,
		`C:\Users\u\AppData\Local\icebath\git\usr\bin\bash.exe`} {
		if !slices.Contains(candidates, want) {
			t.Fatalf("%s missing from %q", want, candidates)
		}
	}
}

func TestCatOverrideAndDefaultProgramFilesLeadTheCandidates(t *testing.T) {
	candidates := WindowsBashCandidates("", map[string]string{"ICEBATH_GIT_BASH_PATH": `E:\bash.exe`})
	if !slices.Equal(candidates[:2], []string{`E:\bash.exe`, `C:\Program Files\Git\bin\bash.exe`}) {
		t.Fatalf("candidates = %q", candidates)
	}
	if got := WindowsBashCandidates(`C:\Windows\System32\..\Tools\bash.exe`, map[string]string{}); got[len(got)-1] != `C:\Windows\System32\..\Tools\bash.exe` {
		t.Fatalf("a path that normalizes out of System32 was dropped: %q", got)
	}
}

func TestCatNonstartingBashIsRejected(t *testing.T) {
	catSwap(t, &runProcess, func(spec processSpec) (processResult, error) { return processResult{Code: 1}, nil })
	if bashStarts("broken-bash.exe") {
		t.Fatal("a bash exiting 1 counts as starting")
	}
}
