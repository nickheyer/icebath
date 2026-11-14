package provider

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Collects secret status lines instead of printing them to stderr
func captureSecretStatus(t *testing.T) *[]string {
	t.Helper()
	var lines []string
	prev := secretStatusPrint
	secretStatusPrint = func(line string) { lines = append(lines, line) }
	t.Cleanup(func() { secretStatusPrint = prev })
	return &lines
}

func forgetCredentialWarning(key string) {
	secretSourceCacheMu.Lock()
	delete(warnedCredentialKeys, key)
	secretSourceCacheMu.Unlock()
}

func TestEnvLoaderSanitizationStripsNonAsciiFromApiKey(t *testing.T) {
	captureSecretStatus(t)
	forgetCredentialWarning("OPENROUTER_API_KEY")
	t.Setenv("OPENROUTER_API_KEY", "sk-proj-abc\u028bdef")
	SanitizeLoadedCredentials()
	if got := os.Getenv("OPENROUTER_API_KEY"); got != "sk-proj-abcdef" {
		t.Fatalf("OPENROUTER_API_KEY %q", got)
	}
}

func TestEnvLoaderSanitizationIgnoresNonCredentialVars(t *testing.T) {
	captureSecretStatus(t)
	t.Setenv("MY_UNICODE_VAR", "h\u00e9llo w\u00f6rld")
	SanitizeLoadedCredentials()
	if got := os.Getenv("MY_UNICODE_VAR"); got != "h\u00e9llo w\u00f6rld" {
		t.Fatalf("MY_UNICODE_VAR %q", got)
	}
}

func TestEnvLoaderSanitizationWarnsToStderrWhenStripping(t *testing.T) {
	lines := captureSecretStatus(t)
	forgetCredentialWarning("GOOGLE_API_KEY")
	t.Setenv("GOOGLE_API_KEY", "AIzaSy\u200babcdef")
	SanitizeLoadedCredentials()
	if got := os.Getenv("GOOGLE_API_KEY"); got != "AIzaSyabcdef" {
		t.Fatalf("GOOGLE_API_KEY %q", got)
	}
	stderr := strings.Join(*lines, "\n")
	if !strings.Contains(stderr, "GOOGLE_API_KEY") || !strings.Contains(stderr, "U+200B") {
		t.Fatalf("stderr %q", stderr)
	}
}

// User home and managed dir of the env_homes fixture, the managed dir in effect for the loader
func envManagedHomes(t *testing.T) (string, string) {
	t.Helper()
	root := envLoaderRoot(t)
	restoreProcessEnv(t)
	h := envLoaderProfile(t, root, "home")
	managed := envLoaderProfile(t, root, "managed")
	t.Setenv(config.EnvManagedDir, managed)
	config.InvalidateManagedCache()
	return h, managed
}

func TestManagedEnvBeatsUserEnv(t *testing.T) {
	h, managed := envManagedHomes(t)
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("OPENAI_API_BASE=https://user.example/v1\n"))
	writeEnvBytes(t, filepath.Join(managed, ".env"), []byte("OPENAI_API_BASE=https://org.example/v1\n"))
	mustLoad(t, DotenvLoadOptions{Home: h})
	if got := os.Getenv("OPENAI_API_BASE"); got != "https://org.example/v1" {
		t.Fatalf("OPENAI_API_BASE %q", got)
	}
}

func TestNoManagedEnvIsNoop(t *testing.T) {
	h, _ := envManagedHomes(t)
	t.Setenv("SOME_VALUE", "from_shell")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("SOME_VALUE=from_user\n"))
	mustLoad(t, DotenvLoadOptions{Home: h})
	if got := os.Getenv("SOME_VALUE"); got != "from_user" {
		t.Fatalf("SOME_VALUE %q", got)
	}
}

const selfRefBasePath = "/usr/bin:/bin"

// Fresh icebath home with PATH pinned to the boot value and multiplex scoping off
func selfRefHome(t *testing.T) string {
	t.Helper()
	root := envLoaderRoot(t)
	restoreProcessEnv(t)
	t.Setenv("PATH", selfRefBasePath)
	t.Setenv("ICEBATH_MULTIPLEX_PROFILES", "0")
	return envLoaderProfile(t, root, "self_ref_home")
}

func TestSameFileReloadedThreeTimesLeavesPathUnchanged(t *testing.T) {
	h := selfRefHome(t)
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("PATH=/x:${PATH}\nPREV=${PATH}\nCHAIN=/c:${PREV}\n"))
	var seen [][2]string
	for i := 0; i < 3; i++ {
		mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: true})
		seen = append(seen, [2]string{os.Getenv("PATH"), os.Getenv("CHAIN")})
	}
	want := [2]string{"/x:" + selfRefBasePath, "/c:/x:" + selfRefBasePath}
	if !reflect.DeepEqual(seen, [][2]string{want, want, want}) {
		t.Fatalf("seen %v", seen)
	}
}

func TestAlternatingProjectEnvAndNoneStaysStable(t *testing.T) {
	h := selfRefHome(t)
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("PATH=/x:${PATH}\n"))
	projectEnv := filepath.Join(t.TempDir(), "project.env")
	writeEnvBytes(t, projectEnv, []byte("UNRELATED=1\n"))
	var counts []int
	for i := 0; i < 6; i++ {
		opts := DotenvLoadOptions{Home: h, SkipExternalSecrets: true}
		if i%2 == 1 {
			opts.ProjectEnv = projectEnv
		}
		mustLoad(t, opts)
		counts = append(counts, strings.Count(os.Getenv("PATH"), "/x:"))
	}
	if !reflect.DeepEqual(counts, []int{1, 1, 1, 1, 1, 1}) {
		t.Fatalf("counts %v", counts)
	}
}

func TestAlternatingHomesEachResolveAgainstBootPath(t *testing.T) {
	root := envLoaderRoot(t)
	restoreProcessEnv(t)
	t.Setenv("PATH", selfRefBasePath)
	t.Setenv("ICEBATH_MULTIPLEX_PROFILES", "0")
	homeA, homeB := envLoaderProfile(t, root, "a"), envLoaderProfile(t, root, "b")
	writeEnvBytes(t, filepath.Join(homeA, ".env"), []byte("PATH=/a:${PATH}\n"))
	writeEnvBytes(t, filepath.Join(homeB, ".env"), []byte("PATH=/b:${PATH}\n"))
	var seen []string
	for _, h := range []string{homeA, homeB, homeA, homeB, homeA} {
		mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: true})
		seen = append(seen, os.Getenv("PATH"))
	}
	want := []string{"/a:" + selfRefBasePath, "/b:" + selfRefBasePath, "/a:" + selfRefBasePath, "/b:" + selfRefBasePath, "/a:" + selfRefBasePath}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen %v", seen)
	}
}

func TestVariableUnsetAtBootAndDefaultSyntaxResolveOnce(t *testing.T) {
	h := selfRefHome(t)
	envLoaderUnset(t, "FOO", "DEF")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("FOO=/f:${FOO}\nDEF=/d:${DEF:-seed}\n"))
	for i := 0; i < 3; i++ {
		mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: true})
	}
	if os.Getenv("FOO") != "/f:" || os.Getenv("DEF") != "/d:seed" {
		t.Fatalf("FOO %q DEF %q", os.Getenv("FOO"), os.Getenv("DEF"))
	}
}

func TestValueChangedOutsideTheLoaderIsNotFrozen(t *testing.T) {
	h := selfRefHome(t)
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("PATH=/x:${PATH}\n"))
	mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: true})
	t.Setenv("PATH", "/opt/new:/bin")
	mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: true})
	mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: true})
	if got := os.Getenv("PATH"); got != "/x:/opt/new:/bin" {
		t.Fatalf("PATH %q", got)
	}
}

func TestLaterLayerInTheSameLoadStillSeesTheEarlierLayer(t *testing.T) {
	h := selfRefHome(t)
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("XPATH=/x:${PATH}\n"))
	projectEnv := filepath.Join(t.TempDir(), "project.env")
	writeEnvBytes(t, projectEnv, []byte("YPATH=/p:${XPATH}\n"))
	for i := 0; i < 3; i++ {
		mustLoad(t, DotenvLoadOptions{Home: h, ProjectEnv: projectEnv, SkipExternalSecrets: true})
	}
	if os.Getenv("XPATH") != "/x:"+selfRefBasePath || os.Getenv("YPATH") != "/p:/x:"+selfRefBasePath {
		t.Fatalf("XPATH %q YPATH %q", os.Getenv("XPATH"), os.Getenv("YPATH"))
	}
}
