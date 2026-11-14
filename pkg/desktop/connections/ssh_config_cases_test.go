package connections

import (
	"errors"
	"sort"
	"strings"
	"testing"
)

func readFrom(files map[string]string) func(string) (string, bool) {
	return func(p string) (string, bool) {
		text, ok := files[strings.ReplaceAll(p, `\`, "/")]
		return text, ok
	}
}

func TestParseSshConfigHostsKeepsLiteralAliasesAndDropsWildcardNegatedPatterns(t *testing.T) {
	cfg := strings.Join([]string{"Host devbox", "  HostName 10.0.0.5", "Host *.internal prod !staging glob*", "Host alpha beta", "# Host commented-out", "host lower-case"}, "\n")
	eq(t, ParseSSHConfigHosts(cfg), []string{"devbox", "prod", "alpha", "beta", "lower-case"})
}

func TestParseSshConfigHostsDeDuplicates(t *testing.T) {
	eq(t, ParseSSHConfigHosts("Host box\nHost box\nHost box other"), []string{"box", "other"})
}

func TestParseSshConfigIncludesExtractsIncludeTokens(t *testing.T) {
	eq(t, ParseSSHConfigIncludes("Include ~/.ssh/config.d/*\nInclude work_hosts personal_hosts\n# Include ignored"), []string{"~/.ssh/config.d/*", "work_hosts", "personal_hosts"})
}

func TestCollectSshConfigHostsFollowsIncludeDirectivesReadOnly(t *testing.T) {
	files := map[string]string{
		"/home/u/.ssh/config": "Host main\nInclude work\nInclude ~/abs_inc",
		"/home/u/.ssh/work":   "Host work-box\nInclude nested",
		"/home/u/.ssh/nested": "Host deep",
		"/home/u/abs_inc":     "Host home-abs",
	}
	hosts := CollectSSHConfigHosts("/home/u/.ssh/config", SSHConfigDeps{HomeDir: "/home/u", ReadFile: readFrom(files)})
	sort.Strings(hosts)
	eq(t, hosts, []string{"deep", "home-abs", "main", "work-box"})
}

func TestCollectSshConfigHostsToleratesAMissingConfigFile(t *testing.T) {
	eq(t, CollectSSHConfigHosts("/nope/config", SSHConfigDeps{HomeDir: "/home/u", ReadFile: func(string) (string, bool) { return "", false }}), []string{})
}

func TestCollectSshConfigHostsDoesNotLoopOnASelfIncludeCycle(t *testing.T) {
	files := map[string]string{"/home/u/.ssh/config": "Host a\nInclude loop", "/home/u/.ssh/loop": "Host b\nInclude config"}
	hosts := CollectSSHConfigHosts("/home/u/.ssh/config", SSHConfigDeps{HomeDir: "/home/u", ReadFile: readFrom(files)})
	sort.Strings(hosts)
	eq(t, hosts, []string{"a", "b"})
}

func TestCollectSshConfigHostsExpandsGlobbedIncludesViaInjectedGlobSync(t *testing.T) {
	files := map[string]string{
		"/home/u/.ssh/config":           "Host root\nInclude config.d/*",
		"/home/u/.ssh/config.d/10-work": "Host work",
		"/home/u/.ssh/config.d/20-home": "Host home",
	}
	hosts := CollectSSHConfigHosts("/home/u/.ssh/config", SSHConfigDeps{HomeDir: "/home/u", ReadFile: readFrom(files), GlobSync: func(pattern string) []string {
		if strings.HasSuffix(strings.ReplaceAll(pattern, `\`, "/"), "config.d/*") {
			return []string{"/home/u/.ssh/config.d/10-work", "/home/u/.ssh/config.d/20-home"}
		}
		return []string{pattern}
	}})
	sort.Strings(hosts)
	eq(t, hosts, []string{"home", "root", "work"})
}

func strp(s string) *string { return &s }

func TestParseSshGOutputPullsHostnameUserPortIdentityfile(t *testing.T) {
	out := strings.Join([]string{"host devbox", "hostname 10.0.0.5", "user alice", "port 2222", "identityfile ~/.ssh/id_ed25519", "forwardagent no"}, "\n")
	port := 2222
	eq(t, ParseSSHGOutput(out), SSHResolved{Hostname: strp("10.0.0.5"), User: strp("alice"), Port: &port, IdentityFile: strp("~/.ssh/id_ed25519")})
}

func TestParseSshGOutputTakesTheFIRSTIdentityfileAndToleratesMissingKeys(t *testing.T) {
	parsed := ParseSSHGOutput("hostname box\nidentityfile ~/.ssh/a\nidentityfile ~/.ssh/b")
	eq(t, *parsed.IdentityFile, "~/.ssh/a")
	eq(t, parsed.User, (*string)(nil))
	eq(t, parsed.Port, (*int)(nil))
}

var sshEnv = WindowsSSHEnv{SystemRoot: `C:\Windows`, LocalAppData: `C:\Users\me\AppData\Local`, ProgramFiles: `C:\Program Files`, ProgramFilesX86: `C:\Program Files (x86)`}

const (
	system32SSH        = `C:\Windows\System32\OpenSSH\ssh.exe`
	programFilesGitSSH = `C:\Program Files\Git\usr\bin\ssh.exe`
	portableGitSSH     = `C:\Users\me\AppData\Local\icebath\git\usr\bin\ssh.exe`
)

type fakeCandidateFS struct {
	present map[string]bool
	dirs    map[string][]string
	probed  []string
}

func newFakeFS(files []string, dirs map[string][]string) *fakeCandidateFS {
	f := &fakeCandidateFS{present: map[string]bool{}, dirs: dirs}
	for _, p := range files {
		f.present[strings.ReplaceAll(p, "/", `\`)] = true
	}
	return f
}

func (f *fakeCandidateFS) Exists(p string) bool {
	n := strings.ReplaceAll(p, "/", `\`)
	f.probed = append(f.probed, n)
	return f.present[n]
}

func (f *fakeCandidateFS) ReadDir(dir string) ([]string, error) {
	entries, ok := f.dirs[strings.ReplaceAll(dir, "/", `\`)]
	if !ok {
		return nil, errors.New("ENOENT: " + dir)
	}
	return entries, nil
}

func TestResolveSshBinaryKeepsBareSshOnNonWindowsPlatformsIgnoresTheOverrideAndNeverProbesTheFilesystem(t *testing.T) {
	for _, platform := range []string{"darwin", "linux", "freebsd"} {
		fs := newFakeFS([]string{system32SSH}, nil)
		eq(t, ResolveSSHBinary(platform, "/opt/ssh", sshEnv, fs), "ssh")
		eq(t, len(fs.probed), 0)
	}
}

func TestResolveSshBinaryReturnsAnExplicitDesktopSshPathOverrideVerbatimEvenWhenItDoesNotExist(t *testing.T) {
	fs := newFakeFS([]string{system32SSH}, nil)
	eq(t, ResolveSSHBinary("win32", `  D:\tools\ssh.exe  `, sshEnv, fs), `D:\tools\ssh.exe`)
	eq(t, len(fs.probed), 0)
}

func TestResolveSshBinaryPrefersTheInBoxSystem32OpenSSHWhenNoOverrideIsSet(t *testing.T) {
	eq(t, ResolveSSHBinary("win32", "", sshEnv, newFakeFS([]string{system32SSH, programFilesGitSSH}, nil)), system32SSH)
}

func TestResolveSshBinaryHonoursANonDefaultSystemRoot(t *testing.T) {
	env := sshEnv
	env.SystemRoot = `D:\Win`
	eq(t, ResolveSSHBinary("win32", "", env, newFakeFS([]string{`D:\Win\System32\OpenSSH\ssh.exe`}, nil)), `D:\Win\System32\OpenSSH\ssh.exe`)
}

func TestResolveSshBinaryFallsBackToGitForWindowsSshExeWhenSystem32OpenSSHIsMissing(t *testing.T) {
	eq(t, ResolveSSHBinary("win32", "", sshEnv, newFakeFS([]string{programFilesGitSSH}, nil)), programFilesGitSSH)
}

func TestResolveSshBinaryPrefersTheIcebathPortableGitSshOverASystemGitInstallMatchingResolveGitBinaryOrder(t *testing.T) {
	eq(t, ResolveSSHBinary("win32", "", sshEnv, newFakeFS([]string{portableGitSSH, programFilesGitSSH}, nil)), portableGitSSH)
}

func TestResolveSshBinaryFallsBackToBareSshPATHLookupWhenNoCandidateExists(t *testing.T) {
	eq(t, ResolveSSHBinary("win32", "", sshEnv, newFakeFS(nil, nil)), "ssh")
}

func TestGitForWindowsSshCandidatesMapsEveryResolveGitBinaryInstallRootToUsrBinSshExeDedupedAndInOrder(t *testing.T) {
	ugitRoot := `C:\Users\me\AppData\Local\UGit`
	ugitGit := ugitRoot + `\app-5.50.1\resources\app\git\cmd\git.exe`
	fs := newFakeFS([]string{ugitGit}, map[string][]string{ugitRoot: {"app-5.50.1"}})
	eq(t, GitForWindowsSSHCandidates(sshEnv, fs), []string{
		portableGitSSH,
		ugitRoot + `\app-5.50.1\resources\app\git\usr\bin\ssh.exe`,
		programFilesGitSSH,
		`C:\Program Files (x86)\Git\usr\bin\ssh.exe`,
		`C:\Users\me\AppData\Local\Programs\Git\usr\bin\ssh.exe`,
	})
}

func TestGitForWindowsSshCandidatesSkipsPerUserCandidatesWhenLOCALAPPDATAIsUnset(t *testing.T) {
	env := sshEnv
	env.LocalAppData = ""
	eq(t, GitForWindowsSSHCandidates(env, newFakeFS(nil, nil)), []string{programFilesGitSSH, `C:\Program Files (x86)\Git\usr\bin\ssh.exe`})
}

func TestSystem32OpenSshMatchesThePathTheDesktopHardCodedBefore103288(t *testing.T) {
	eq(t, System32OpenSSH(`C:\Windows`), system32SSH)
	eq(t, System32OpenSSH(""), system32SSH)
}
