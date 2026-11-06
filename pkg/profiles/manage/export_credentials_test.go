package manage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

const leakedKey = "sk-or-v1-reallyLongSecretKeyValue12345678"

// Credential stores of the export tests beyond PROFILE_CREDENTIAL_PATHS
var extraCredentialStores = []string{
	".op.env", "npmrc", "honcho.json", "google_chat_user_token.json", "google_chat_user_oauth_pending",
	"workspace/meetings/node_token.json", "weixin/accounts", ".copilot_jwt.json", "proxy", "chrome-debug",
	"home/.git-credentials", "home/.config/gh/hosts.yml", "backups", "state-snapshots",
	"teams_pipeline_store.json", "mem0.json",
	"browser-profiles/live/Default/Cookies", "browser_profiles/default/Default/Login Data",
	"mcp-tokens/srv.json", "vault/vault.key", "platforms/pairing/approved.json", "slack_tokens.json",
	"webhook_subscriptions.json", ".ssh/id_rsa", ".aws/credentials", ".gnupg/x", ".kube/config", ".envrc",
	"webapp/session-token", "webapp/native-sign-in-tokens.json", "webapp/sign-in-registration.json",
}

// HOME_CREDENTIAL_DIRS of agent/file_safety.py, the stores seeded as directories
var homeCredentialDirSet = map[string]bool{".ssh": true, ".aws": true, ".gnupg": true, ".kube": true,
	".docker": true, ".azure": true, ".config/gh": true, ".config/gcloud": true}

// Lays every credential store into a profile shaped tree at root and returns their relative paths
func seedCredentialStores(t *testing.T, root string) map[string]bool {
	t.Helper()
	stores := map[string]bool{}
	for _, r := range extraCredentialStores {
		stores[r] = true
	}
	for r := range profiles.ProfileCredentialPaths {
		stores[r] = true
	}
	if err := os.MkdirAll(filepath.Join(root, "platforms"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "config.yaml"), "model: gpt-4\n")
	writeFile(t, filepath.Join(root, "platforms", "keep.json"), "{}")
	for rel := range stores {
		last := rel[strings.LastIndex(rel, "/")+1:]
		isDir := homeCredentialDirSet[rel] || (!strings.Contains(last, ".") && rel != "npmrc" && rel != "webapp/session-token")
		target := filepath.Join(root, filepath.FromSlash(rel))
		if isDir {
			target = filepath.Join(target, "store")
		}
		writeFile(t, target, "fake-credential")
	}
	return stores
}

// Regular file members of a tar.gz archive keyed by name
func archiveMembers(t *testing.T, archive string) (map[string]string, []string) {
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
	files := map[string]string{}
	var names []string
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			files[h.Name] = string(data)
		}
	}
	return files, names
}

// Named profile directory under the test home without the identity marker namedProfile writes
func bareNamedProfile(t *testing.T, tmp, name string) string {
	t.Helper()
	dir := filepath.Join(tmp, ".icebath", "profiles", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNamedProfileExportExcludesAuth(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	dir := bareNamedProfile(t, tmp, "testprofile")
	writeFile(t, filepath.Join(dir, "config.yaml"), "model: gpt-4\n")
	writeFile(t, filepath.Join(dir, "auth.json"), `{"tokens": {"access": "sk-secret"}}`)
	writeFile(t, filepath.Join(dir, ".env"), "OPENROUTER_API_KEY=sk-secret-key\n")
	writeFile(t, filepath.Join(dir, "SOUL.md"), "I am helpful.\n")
	writeFile(t, filepath.Join(dir, "memories", "MEMORY.md"), "# Memories\n")
	result, err := ExportProfile("testprofile", filepath.Join(tmp, "export.tar.gz"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, names := archiveMembers(t, result)
	has := func(sub string) bool {
		return slices.ContainsFunc(names, func(n string) bool { return strings.Contains(n, sub) })
	}
	if !has("config.yaml") || !has("SOUL.md") {
		t.Fatalf("config.yaml and SOUL.md should be in export: %v", names)
	}
	if has("auth.json") || has(".env") {
		t.Fatalf("auth.json and .env must NOT be in export: %v", names)
	}
}

func TestDistributionCanNeitherPlantNorReplaceAStore(t *testing.T) {
	for _, declareOwned := range []bool{false, true} {
		t.Run(map[bool]string{false: "undeclared", true: "declared"}[declareOwned], func(t *testing.T) {
			tmp, _ := distEnv(t)
			captureOut(t)
			stores := seedCredentialStores(t, filepath.Join(tmp, "v1"))
			for _, c := range []string{"auth.json.corrupt", ".env.bak-20260928T084018Z", "config.yaml.bak-20260928T084018Z"} {
				stores[c] = true
				writeFile(t, filepath.Join(tmp, "v1", c), "fake-credential")
			}
			var owned []string
			if declareOwned {
				set := map[string]bool{"SOUL.md": true, "PLATFORMS/PAIRING": true}
				for r := range stores {
					set[r] = true
					set[strings.Split(r, "/")[0]] = true
				}
				for r := range set {
					owned = append(owned, r)
				}
				slices.Sort(owned)
			}
			stage := func(label, soul string) string {
				staged := filepath.Join(tmp, label)
				writeFile(t, filepath.Join(staged, "SOUL.md"), soul)
				m := profiles.NewDistributionManifest("dist")
				m.Version, m.DistributionOwned = "0.1.0", owned
				if _, err := profiles.WriteManifest(staged, m); err != nil {
					t.Fatal(err)
				}
				return staged
			}
			installed := mustInstall(t, stage("v1", "v1"), "dist", false).TargetDir
			if !exists(filepath.Join(installed, "platforms", "keep.json")) {
				t.Fatal("platforms/keep.json did not install")
			}
			var planted []string
			for r := range stores {
				p := filepath.Join(installed, filepath.FromSlash(r))
				_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
					if err == nil && d.Type().IsRegular() {
						if data, rerr := os.ReadFile(path); rerr == nil && string(data) == "fake-credential" {
							planted = append(planted, r)
							return filepath.SkipAll
						}
					}
					return nil
				})
			}
			if len(planted) > 0 {
				t.Fatalf("planted %v", planted)
			}
			live := []string{filepath.Join(installed, "platforms", "pairing", "approved.json"),
				filepath.Join(installed, "platforms", "whatsapp", "session", "creds.json")}
			for _, store := range live {
				if err := os.MkdirAll(filepath.Dir(filepath.Dir(store)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Dir(store), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, store, "installer-credential")
			}
			liveIntact := func() {
				t.Helper()
				for _, s := range live {
					if readText(t, s) != "installer-credential" {
						t.Fatalf("%s replaced", s)
					}
				}
			}
			for i, shipped := range []string{"platforms", "platforms/whatsapp"} {
				update := stage([]string{"v2-0", "v2-1"}[i], "v2")
				writeFile(t, filepath.Join(update, filepath.FromSlash(shipped)), "not a directory")
				_, err := InstallDistribution(context.Background(), update, "dist", true, false)
				var de *profiles.DistributionError
				if !errors.As(err, &de) {
					t.Fatalf("%s: want DistributionError, got %v", shipped, err)
				}
				liveIntact()
				if readText(t, filepath.Join(installed, "SOUL.md")) != "v1" {
					t.Fatal("SOUL.md changed by a refused update")
				}
			}
			update := stage("v2-dir", "v2")
			writeFile(t, filepath.Join(update, "platforms", "whatsapp", "config.json"), "{}")
			mustInstall(t, update, "dist", true)
			if !exists(filepath.Join(installed, "platforms", "whatsapp", "config.json")) {
				t.Fatal("platforms/whatsapp/config.json did not install")
			}
			liveIntact()
		})
	}
}

func TestNamedProfileExportRedactsSecretsInText(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	dir := bareNamedProfile(t, tmp, "scrubme")
	soul := filepath.Join(dir, "SOUL.md")
	skill := filepath.Join(dir, "skills", "demo", "SKILL.md")
	memory := filepath.Join(dir, "memories", "MEMORY.md")
	writeFile(t, soul, "My key is "+leakedKey+"\n")
	writeFile(t, skill, "---\nname: demo\ndescription: Demo.\n---\nUse OPENROUTER_API_KEY="+leakedKey+"\n")
	writeFile(t, memory, "token "+leakedKey+"\n")
	writeFile(t, filepath.Join(dir, "config.yaml"), "model: gpt-4\n")
	result, err := ExportProfile("scrubme", filepath.Join(tmp, "scrubme.tar.gz"), nil)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := archiveMembers(t, result)
	var blob []string
	seen := map[string]bool{}
	for name, body := range files {
		if strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".yaml") {
			blob = append(blob, body)
			seen[filepath.Base(name)] = true
		}
	}
	if strings.Contains(strings.Join(blob, "\n"), leakedKey) {
		t.Fatal("leaked key in the archive")
	}
	if !seen["SOUL.md"] || !seen["SKILL.md"] || !seen["MEMORY.md"] {
		t.Fatalf("members %v", seen)
	}
	for _, p := range []string{soul, skill, memory} {
		if !strings.Contains(readText(t, p), leakedKey) {
			t.Fatalf("%s source was scrubbed", p)
		}
	}
}

func TestExportRedactsThroughSymlinkWithoutTouchingSource(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	dir := bareNamedProfile(t, tmp, "linkme")
	outside := filepath.Join(tmp, "outside-skill.md")
	writeFile(t, outside, "secret "+leakedKey+"\n")
	link := filepath.Join(dir, "skills", "linked", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	writeFile(t, filepath.Join(dir, "config.yaml"), "model: gpt-4\n")
	result, err := ExportProfile("linkme", filepath.Join(tmp, "linkme.tar.gz"), nil)
	if err != nil {
		t.Fatal(err)
	}
	files, names := archiveMembers(t, result)
	var archived string
	found := false
	for _, n := range names {
		if strings.HasSuffix(n, "SKILL.md") {
			archived, found = files[n], true
			break
		}
	}
	if !found || strings.Contains(archived, leakedKey) {
		t.Fatalf("skill member %v %q", found, archived)
	}
	if !strings.Contains(readText(t, outside), leakedKey) {
		t.Fatal("source was scrubbed")
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&fs.ModeSymlink == 0 {
		t.Fatal("link replaced")
	}
}

func TestCloneAllLeavesTheSourceGatewaysWebappSignInStateBehind(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	def := filepath.Join(tmp, ".icebath")
	writeFile(t, filepath.Join(def, "config.yaml"), "model: test")
	record := filepath.Join(def, "webapp", "native-sign-in-tokens.json")
	writeFile(t, record, `{"`+strings.Repeat("d", 64)+`": 4102444800}`+"\n")
	writeFile(t, filepath.Join(def, "webapp", "sign-in-registration.json"),
		`{"portal": "https://portal.example.test", "client_id": "c1", "callback": "http://192.168.1.50:9120/auth/callback"}`+"\n")
	dir, err := CreateProfile(context.Background(), "coder", CreateOptions{CloneAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "webapp")) {
		entries, _ := os.ReadDir(filepath.Join(dir, "webapp"))
		t.Fatalf("webapp cloned %v", entries)
	}
	if !exists(record) {
		t.Fatal("source record removed")
	}
}
