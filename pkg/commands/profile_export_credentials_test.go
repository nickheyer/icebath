package commands

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

const exportLeakedKey = "sk-or-v1-reallyLongSecretKeyValue12345678"

// Credential stores named beside PROFILE_CREDENTIAL_PATHS so dropping one from the list fails the case
var exportExtraStores = []string{
	".op.env", "npmrc", "honcho.json", "google_chat_user_token.json", "google_chat_user_oauth_pending",
	"workspace/meetings/node_token.json", "weixin/accounts", ".copilot_jwt.json", "proxy", "chrome-debug",
	"home/.git-credentials", "home/.config/gh/hosts.yml", "backups", "state-snapshots",
	"teams_pipeline_store.json", "mem0.json",
	"browser-profiles/live/Default/Cookies", "browser_profiles/default/Default/Login Data",
	"mcp-tokens/srv.json", "vault/vault.key", "platforms/pairing/approved.json", "slack_tokens.json",
	"webhook_subscriptions.json", ".ssh/id_rsa", ".aws/credentials", ".gnupg/x", ".kube/config", ".envrc",
	"webapp/session-token", "webapp/native-sign-in-tokens.json", "webapp/sign-in-registration.json",
}

// Dot named stores seeded as directories, the HOME_CREDENTIAL_DIRS of file_safety
var exportDotDirs = map[string]bool{".ssh": true, ".aws": true, ".gnupg": true, ".kube": true, ".docker": true,
	".azure": true, ".config/gh": true, ".config/gcloud": true}

// Lays every credential store into a profile shaped tree at root and returns their relative paths
func seedExportStores(t *testing.T, root string) map[string]bool {
	t.Helper()
	stores := map[string]bool{}
	for _, r := range exportExtraStores {
		stores[r] = true
	}
	for r := range profiles.ProfileCredentialPaths {
		stores[r] = true
	}
	writeTestFile(t, filepath.Join(root, "config.yaml"), []byte("model: gpt-4\n"))
	writeTestFile(t, filepath.Join(root, "platforms", "keep.json"), []byte("{}"))
	for rel := range stores {
		last := rel[strings.LastIndex(rel, "/")+1:]
		isDir := exportDotDirs[rel] || (!strings.Contains(last, ".") && rel != "npmrc" && rel != "webapp/session-token")
		target := filepath.Join(root, filepath.FromSlash(rel))
		if isDir {
			target = filepath.Join(target, "store")
		}
		writeTestFile(t, target, []byte("fake-credential"))
	}
	return stores
}

// Runs a step whose hermes code is still owed, failing the case with the owed name
func owedStep(t *testing.T, step func() (string, error)) (out string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%v", r)
		}
	}()
	v, err := step()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Member names of a tar.gz archive and the text of one member
func tarMembers(t *testing.T, archive, want string) (map[string]bool, string) {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	names, text := map[string]bool{}, ""
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names, text
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
		if h.Name == want {
			data, _ := io.ReadAll(tr)
			text = string(data)
		}
	}
}

func TestCredentialExclusionNamedExportShipsNoCredentialStoreOrCopyOfOne(t *testing.T) {
	tmp := profileTestHome(t)
	var out strings.Builder
	t.Cleanup(manage.SwapOutput(&out, &out))
	profileDir := filepath.Join(tmp, "profiles", "testprofile")
	stores := seedExportStores(t, profileDir)
	writeTestFile(t, filepath.Join(profileDir, "config.yaml"), []byte("model:\n  api_key: "+exportLeakedKey+"\n"))
	writeTestFile(t, filepath.Join(profileDir, "config.yaml.bak-my-note"), []byte("model:\n  api_key: "+exportLeakedKey+"\n"))
	writeTestFile(t, filepath.Join(profileDir, "GOOGLE_CHAT_USER_TOKENS", "upper.json"), []byte("fake-credential"))
	nested := []string{}
	for _, r := range []string{".ssh/id_rsa", ".aws/credentials", ".gnupg/x", ".kube/config", ".envrc", ".docker/config.json",
		".azure/accessTokens.json", ".config/gh/hosts.yml", ".config/gcloud/application_default_credentials.json"} {
		nested = append(nested, "skills/s/"+r)
	}
	for _, rel := range append(append([]string{}, nested...), "skills/s/.config/other/settings.json") {
		writeTestFile(t, filepath.Join(profileDir, filepath.FromSlash(rel)), []byte("fake-credential"))
	}
	t.Setenv(home.EnvHome, profileDir)
	home.ResetKeyCache()
	copies := []string{owedStep(t, func() (string, error) { return backup.CreatePreUpdateBackup(profileDir, 5) })}
	snap := owedStep(t, func() (string, error) { return backup.CreateQuickSnapshot(nil, profileDir, nil, nil) })
	copies = append(copies, filepath.Join(profileDir, "state-snapshots", snap))
	copies = append(copies, owedStep(t, func() (string, error) {
		return config.BackupConfig(context.Background(), filepath.Join(profileDir, "config.yaml"), "setup", 5)
	}))
	baks, err := selfupdate.BackupExisting([]string{filepath.Join(profileDir, ".env"), filepath.Join(profileDir, "config.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	copies = append(copies, baks...)
	if _, err := provider.NewStore(profileDir).LoadFile(filepath.Join(profileDir, "auth.json")); err != nil {
		t.Fatal(err)
	}
	copies = append(copies, filepath.Join(profileDir, "auth.json.corrupt"))
	for _, c := range copies {
		if _, err := os.Stat(c); c == "" || err != nil {
			t.Fatalf("copy %q %v of %v", c, err, copies)
		}
	}
	t.Cleanup(manage.SwapExistingProfileDir(func(name string) (string, string, error) { return name, profileDir, nil }))
	archive, err := manage.ExportProfile("testprofile", filepath.Join(tmp, "export.tar.gz"), nil)
	if err != nil {
		t.Fatal(err)
	}
	members, note := tarMembers(t, archive, "testprofile/config.yaml.bak-my-note")
	defaultArchive, err := manage.ExportProfile("default", filepath.Join(tmp, "default.tar.gz"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defaultMembers, _ := tarMembers(t, defaultArchive, "")
	for _, want := range []string{"testprofile/config.yaml", "testprofile/platforms/keep.json", "testprofile/skills/s/.config/other/settings.json"} {
		if !members[want] {
			t.Fatalf("%s missing", want)
		}
	}
	if !defaultMembers["default/config.yaml"] || !defaultMembers["default/skills/s/.config/other/settings.json"] {
		t.Fatal("default export lost user data")
	}
	if strings.Contains(note, exportLeakedKey) {
		t.Fatal("hand named config copy shipped unscrubbed")
	}
	rels := map[string]bool{"google_chat_user_tokens/upper.json": true}
	for r := range stores {
		rels[r] = true
	}
	for _, r := range nested {
		rels[r] = true
	}
	for _, c := range copies {
		rel, err := filepath.Rel(profileDir, c)
		if err != nil {
			t.Fatal(err)
		}
		rels[filepath.ToSlash(rel)] = true
	}
	folded := []string{}
	for n := range members {
		folded = append(folded, profiles.Casefold(n))
	}
	for n := range defaultMembers {
		folded = append(folded, profiles.Casefold(n))
	}
	var leaked []string
	for _, p := range []string{"testprofile", "default"} {
		for r := range rels {
			full := profiles.Casefold(p + "/" + r)
			for _, n := range folded {
				if n == full || strings.HasPrefix(n, full+"/") {
					leaked = append(leaked, fmt.Sprintf("%s/%s", p, r))
					break
				}
			}
		}
	}
	sort.Strings(leaked)
	if len(leaked) > 0 {
		t.Fatalf("leaked %v", leaked)
	}
}
