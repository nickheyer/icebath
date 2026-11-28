package profile

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tools/redact"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Stores in a profile's folder that hold credentials, never copied into an export, an archive or a distribution
var credentialStores = []string{"auth.json", ".env", "auth/google_oauth.json", ".op.env", "npmrc", ".anthropic_oauth.json", "google_token.json",
	"google_oauth_pending.json", "google_client_secret.json", "google_chat_user_tokens", "google_chat_user_client_secret.json",
	"google_chat_user_oauth_pending.json", "google_chat_user_token.json", "google_chat_user_oauth_pending", "slack_tokens.json",
	"webhook_subscriptions.json", "teams_pipeline_store.json", "mcp-tokens", "vault", "browser-profile", "browser_auth", "bot-desktop", "webapp",
	"browser-profiles", "browser_profiles", "pairing", "platforms/pairing", "feishu_comment_pairing.json", "whatsapp/session",
	"platforms/whatsapp/session", "matrix/store", "platforms/matrix/store", "workspace/meetings/node_token.json", "weixin/accounts",
	".copilot_jwt.json", "proxy", "chrome-debug", "home", "backups", "state-snapshots",
	"bws_cache.json", "bws_cache.enc.json", "op_cache.json", "photon-sidecar.json", "cache/bws_cache.json", "cache/bws_cache.enc.json",
	"cache/op_cache.json", "runtime/photon-sidecar.json",
	".ssh", ".aws", ".gnupg", ".kube", ".docker", ".azure", ".config/gh", ".config/gcloud", ".envrc"}

// Copies of a credential store left at the top of a profile
var storeCopy = regexp.MustCompile(`^(?:auth\.json\..+|\.env\.bak.*|config\.yaml\.(?:bak-\d{8}t\d{6}z(?:\.\d+)?|bak\.\d+|corrupt\..*|bak-pre-migrate-.*))$`)

// Trees of the package manager at the top of a profile, belonging to this machine alone
var runtimeTrees = map[string]bool{"installs": true, "tools": true, "cache": true}

// Text files an export masks the secrets of, by suffix
var textSuffixes = map[string]bool{".md": true, ".txt": true, ".yaml": true, ".yml": true, ".json": true, ".jsonl": true, ".toml": true, ".ini": true,
	".cfg": true, ".conf": true, ".py": true, ".sh": true, ".bash": true, ".zsh": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
	".css": true, ".html": true, ".xml": true, ".csv": true}

func folded(parts []string) []string {
	out := make([]string, len(parts))
	for i, part := range parts {
		out[i] = strings.ToLower(part)
	}
	return out
}

// Whether a path of a profile, by its parts, is a credential store, lies in one, or is a copy of one at the top
func private(parts []string) bool {
	parts = folded(parts)
	if len(parts) == 1 && storeCopy.MatchString(parts[0]) {
		return true
	}
	for _, store := range credentialStores {
		storeParts := strings.Split(strings.ToLower(store), "/")
		if len(storeParts) <= len(parts) && slices.Equal(storeParts, parts[:len(storeParts)]) {
			return true
		}
	}
	return false
}

// Whether a path below a profile's folder, slash separated, is a credential store, lies in one, or is a copy of one: what no export carries
// and no copy made without credentials takes
func credentialStore(rel string) bool {
	return private(parts(rel)) || buried(parts(rel))
}

// Stores no export carries wherever they lie in a profile, not only at its top: logins, keys and the credential folders of an account
var buriedStores = []string{"auth.json", ".env", "bot-desktop", ".ssh", ".aws", ".gnupg", ".kube", ".docker", ".azure", ".config/gh", ".config/gcloud",
	".envrc"}

// Whether a path of a profile, by its parts, ends in one of the stores no export carries at any depth
func buried(parts []string) bool {
	for _, store := range buriedStores {
		storeParts := strings.Split(store, "/")
		if len(storeParts) <= len(parts) && slices.Equal(parts[len(parts)-len(storeParts):], storeParts) {
			return true
		}
	}
	return false
}

// Whether a path of a profile, by its parts, holds a credential store somewhere below it
func holdsStore(parts []string) bool {
	parts = folded(parts)
	for _, store := range credentialStores {
		storeParts := strings.Split(strings.ToLower(store), "/")
		if len(storeParts) > len(parts) && slices.Equal(parts, storeParts[:len(parts)]) {
			return true
		}
	}
	return false
}

// Whether an entry is never copied out of a profile: caches, sockets, temporary files and anything but a file, a folder or a link
func unexportable(entry fs.DirEntry) bool {
	name := entry.Name()
	if name == "__pycache__" || strings.HasSuffix(name, ".sock") || strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".pyc") ||
		strings.HasSuffix(name, ".pyo") {
		return true
	}
	mode := entry.Type()
	return !(mode.IsRegular() || mode.IsDir() || mode&fs.ModeSymlink != 0)
}

// Copies a tree, links as links and modes kept, skipping what skip says by the slash separated path below the source
func copyTree(src, dst string, skip func(rel string, entry fs.DirEntry) bool) error {
	return filepath.WalkDir(src, func(at string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, at)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if rel == "." {
			return os.MkdirAll(target, 0o755)
		}
		if skip(filepath.ToSlash(rel), entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(at)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case entry.IsDir():
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		return fsx.Copy(at, target)
	})
}

// Parts of a slash separated path
func parts(rel string) []string {
	return strings.Split(rel, "/")
}

// Whether an export masks the secrets of the file by its name
func textFile(name string) bool {
	lower := strings.ToLower(name)
	return name == ".cursorrules" || strings.HasSuffix(lower, ".env.example") || strings.HasPrefix(lower, "config.yaml.") || textSuffixes[path.Ext(lower)]
}

// Names whose values are credentials, by the last part of their key
var (
	secretNames    = []string{"api_key", "apikey", "key", "token", "access_token", "refresh_token", "id_token", "secret", "client_secret", "password", "passwd", "authorization", "private_key", "bearer", "jwt"}
	secretEndings  = []string{"_api_key", "_token", "_secret", "_password", "_key", "_access_key"}
	secretBlocking = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)
)

func secretName(key string) bool {
	leaf := strings.ReplaceAll(strings.ToLower(key), "-", "_")
	return slices.Contains(secretNames, leaf) || slices.ContainsFunc(secretEndings, func(ending string) bool { return strings.HasSuffix(leaf, ending) })
}

// Whether the value kept under the key of the settings is a credential: a setting marked secret, or for a key no setting is registered for
// one named like a credential; an empty value or a reference to a variable never is
func credential(cfg *config.Config, key string, value *yaml.Node) bool {
	// An alias is what it stands for
	if value.Kind == yaml.AliasNode && value.Alias != nil {
		value = value.Alias
	}
	if value.Kind == yaml.ScalarNode && (value.Value == "" || value.Tag == "!!null" || secretBlocking.MatchString(value.Value)) {
		return false
	}
	if _, setting, known := cfg.Setting(key); known {
		return setting.Secret
	}
	leaf := key[strings.LastIndex(key, ".")+1:]
	return value.Kind != yaml.MappingNode && secretName(leaf)
}

// Takes every credential out of a mapping of the settings, those below it included, the rest kept in its order
func stripSecrets(cfg *config.Config, mapping *yaml.Node, prefix string) {
	for i := 0; i+1 < len(mapping.Content); {
		key, value := mapping.Content[i].Value, mapping.Content[i+1]
		if prefix != "" {
			key = prefix + "." + key
		}
		if credential(cfg, key, value) {
			mapping.Content = slices.Delete(mapping.Content, i, i+2)
			continue
		}
		if value.Kind == yaml.MappingNode {
			stripSecrets(cfg, value, key)
		}
		i += 2
	}
}

// What an export writes of a file: its config.yaml without credentials, other text files with what looks like a secret masked
func scrub(cfg *config.Config, rel string, data []byte) []byte {
	if !textFile(path.Base(rel)) || !utf8.Valid(data) {
		return data
	}
	data = bytes.TrimPrefix(data, bom)
	if rel == "config.yaml" {
		if stripped, err := config.Without(data, func(settings *yaml.Node) { stripSecrets(cfg, settings, "") }); err == nil {
			data = stripped
		}
	}
	return []byte(redact.File(string(data), true))
}

// Archive of a profile's folder under one top folder named for it, without its credentials, the stores of this machine's package manager or
// what is never copied, with its text masked of secrets
func (p *Profiles) Export(name string) ([]byte, string, error) {
	canon, err := Canon(name)
	if err != nil {
		return nil, "", err
	}
	if !p.Exists(canon) {
		return nil, "", unknown(canon)
	}
	dir, settings := p.Dir(canon), p.config.In(canon)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	err = filepath.WalkDir(dir, func(at string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, at)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if unexportable(entry) || credentialStore(rel) || !strings.Contains(rel, "/") && runtimeTrees[rel] {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Lstat(at)
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(at); err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = canon + "/" + rel
		if info.IsDir() {
			header.Name += "/"
			return tw.WriteHeader(header)
		}
		if !info.Mode().IsRegular() {
			return tw.WriteHeader(header)
		}
		data, err := os.ReadFile(at)
		if err != nil {
			return err
		}
		data = scrub(settings, rel, data)
		header.Size = int64(len(data))
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		return nil, "", err
	}
	return buf.Bytes(), canon + "-" + time.Now().Format("20060102-150405") + ".tar.gz", nil
}

// Path of a member of an archive below the folder it unpacks into, an error for one that would land elsewhere
func memberPath(name string) (string, error) {
	cleaned := path.Clean(strings.ReplaceAll(name, `\`, "/"))
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") || filepath.VolumeName(cleaned) != "" {
		return "", &ValueError{"The archive holds " + name + ", which points outside the archive, so nothing was imported."}
	}
	return cleaned, nil
}

// Absolute path with the links of the part of it that exists resolved and the rest kept as it is
func resolveLoose(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	for at, tail := abs, ""; ; {
		if resolved, err := filepath.EvalSymlinks(at); err == nil {
			return filepath.Join(resolved, tail)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return abs
		}
		parent := filepath.Dir(at)
		if parent == at {
			return abs
		}
		tail = filepath.Join(filepath.Base(at), tail)
		at = parent
	}
}

// Whether a link in the folder holding the text leads into the root, followed as the system follows it: each .. applying to what the parts
// before it resolve to
func leadsInside(dir, link, root string) bool {
	if filepath.IsAbs(link) || strings.HasPrefix(link, "/") || strings.HasPrefix(link, `\`) || filepath.VolumeName(link) != "" {
		return false
	}
	at := resolveLoose(dir)
	for _, part := range strings.FieldsFunc(link, func(r rune) bool { return r == '/' || r == '\\' }) {
		switch part {
		case ".":
		case "..":
			at = filepath.Dir(at)
		default:
			at = resolveLoose(filepath.Join(at, part))
		}
	}
	return fsx.Within(at, root)
}

// Permissions an unpacked file gets: no set-id or sticky bits, no write for group and others, execute only where its owner had it, read
// and write for its owner always
func safeMode(mode int64) fs.FileMode {
	m := fs.FileMode(mode) & 0o755
	if m&0o100 == 0 {
		m &^= 0o111
	}
	return m | 0o600
}

// Removes what stands where a member goes unless it is a folder
func clearTarget(target string) error {
	if info, err := os.Lstat(target); err == nil && !info.IsDir() {
		return os.Remove(target)
	}
	return nil
}

// Refusal of an archive member that would land or lead outside the folder it unpacks into
func escapes(name string) error {
	return &ValueError{fmt.Sprintf("The archive holds %s, a link that leads outside the archive, so nothing was imported.", name)}
}

// Unpacks a profile archive into the folder: files, folders and links that stay inside it, every link checked again once all are in, and no
// more content than the folder's volume has room for; the top folders it holds
func unpack(data []byte, dest string) (map[string]bool, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, &ValueError{"This is not a profile archive, which is a .tar.gz file: " + strs.Sentence(err.Error())}
	}
	root, err := filepath.EvalSymlinks(dest)
	if err != nil {
		return nil, err
	}
	space := room(root)
	tr := tar.NewReader(zr)
	tops := map[string]bool{}
	// A link or folder made, the member it came from, the top folder it must stay in and the folder's time to set once its entries are in
	type made struct {
		path, name, top string
		at              time.Time
	}
	var links, dirs []made
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, &ValueError{"The profile archive can't be read: " + strs.Sentence(err.Error())}
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		rel, err := memberPath(header.Name)
		if err != nil {
			return nil, err
		}
		if rel == "." {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(rel))
		// The folder a member goes in, its links resolved, lies in the root before anything is made for it
		if !fsx.Within(resolveLoose(filepath.Dir(target)), root) {
			return nil, escapes(header.Name)
		}
		top, _, _ := strings.Cut(rel, "/")
		tops[top] = true
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if !fsx.Within(resolveLoose(target), root) {
				return nil, escapes(header.Name)
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
			dirs = append(dirs, made{path: target, name: header.Name, at: header.ModTime})
		case tar.TypeReg, tar.TypeCont, tar.TypeGNUSparse:
			if space >= 0 {
				if header.Size > space {
					return nil, &ValueError{fmt.Sprintf("%s in the archive does not fit in the space left on this disk.", header.Name)}
				}
				space -= header.Size
			}
			if err := clearTarget(target); err != nil {
				return nil, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return nil, err
			}
			err = out.Chmod(safeMode(header.Mode))
			if err == nil {
				_, err = io.Copy(out, tr)
			}
			if closeErr := out.Close(); err == nil {
				err = closeErr
			}
			if err == nil {
				err = os.Chtimes(target, header.ModTime, header.ModTime)
			}
			if err != nil {
				return nil, err
			}
		case tar.TypeSymlink:
			// A link stays in the folder of the profile it belongs to, which is all that moves into place
			if !leadsInside(filepath.Dir(target), header.Linkname, filepath.Join(root, top)) {
				return nil, escapes(header.Name)
			}
			if err := clearTarget(target); err != nil {
				return nil, err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return nil, err
			}
			links = append(links, made{path: target, name: header.Name, top: filepath.Join(root, top)})
		case tar.TypeLink:
			linked, err := memberPath(header.Linkname)
			if err != nil {
				return nil, escapes(header.Name)
			}
			// Linked through every link resolved, so the second name never lands on what lies outside
			source := resolveLoose(filepath.Join(root, filepath.FromSlash(linked)))
			if !fsx.Within(source, filepath.Join(root, top)) {
				return nil, escapes(header.Name)
			}
			info, err := os.Lstat(source)
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				return nil, &ValueError{fmt.Sprintf("%s in the archive is a link to something that is not a file.", header.Name)}
			}
			if have, err := os.Lstat(target); err != nil || !os.SameFile(info, have) {
				if err := clearTarget(target); err != nil {
					return nil, err
				}
				if err := os.Link(source, target); err != nil {
					return nil, err
				}
			}
			if err := os.Chmod(target, safeMode(header.Mode)); err != nil {
				return nil, err
			}
		default:
			return nil, &ValueError{fmt.Sprintf("%s in the archive is not a file, a folder or a link.", header.Name)}
		}
	}
	// A later member can replace a folder a link passes through with a link of its own: every link must still lead inside
	var outside error
	for _, link := range links {
		text, err := os.Readlink(link.path)
		if err != nil || leadsInside(filepath.Dir(link.path), text, link.top) {
			continue
		}
		os.Remove(link.path)
		if outside == nil {
			outside = escapes(link.name)
		}
	}
	if outside != nil {
		return nil, outside
	}
	slices.SortFunc(dirs, func(a, b made) int { return strings.Compare(b.path, a.path) })
	for _, dir := range dirs {
		if info, err := os.Lstat(dir.path); err == nil && info.IsDir() {
			os.Chtimes(dir.path, dir.at, dir.at)
		}
	}
	return tops, nil
}

// Makes a profile of an archive holding one top folder, named as asked or for that folder, without the stores of another machine's package
// manager, its config.yaml and .env kept to their owner
func (p *Profiles) Import(data []byte, name string) (string, error) {
	staging, err := os.MkdirTemp("", "icebath_profile_import_")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	tops, err := unpack(data, staging)
	if err != nil {
		return "", err
	}
	root := ""
	if len(tops) == 1 {
		for top := range tops {
			root = top
		}
	}
	inferred := name
	if inferred == "" {
		inferred = root
	}
	if inferred == "" {
		return "", &ValueError{"icebath can't tell the name of the profile from the archive. Give it with icebath profile import <archive> --name NAME."}
	}
	if root == "" {
		return "", &ValueError{"A profile archive must hold exactly one folder at its top."}
	}
	canon, err := Canon(inferred)
	if err != nil {
		return "", err
	}
	if canon == Default {
		return "", &ValueError{fmt.Sprintf("An archive can't be imported as default, the built-in profile in %s. Give another name with icebath "+
			"profile import <archive> --name NAME.", p.Dir(Default))}
	}
	dir := p.Dir(canon)
	if fsx.Lexists(dir) {
		return "", taken(canon)
	}
	extracted := filepath.Join(staging, root)
	if !fsx.IsDir(extracted) {
		return "", &ValueError{"The folder at the top of the archive, " + root + ", is missing or is not a folder."}
	}
	for tree := range runtimeTrees {
		if err := os.RemoveAll(filepath.Join(extracted, tree)); err != nil {
			return "", err
		}
	}
	for _, file := range []string{"config.yaml", ".env"} {
		if info, err := os.Lstat(filepath.Join(extracted, file)); err == nil && info.Mode().IsRegular() {
			if err := os.Chmod(filepath.Join(extracted, file), 0o600); err != nil {
				return "", err
			}
		}
	}
	if err := os.MkdirAll(p.root(), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(extracted, dir); err != nil {
		if err := copyTree(extracted, dir, func(string, fs.DirEntry) bool { return false }); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}

// Saves a profile's archive where asked, always as a .tar.gz, else under the file name its export gave it in the folder exports go to of
// icebath's folder on this machine: where it went
func SaveArchive(data []byte, file, output string) (string, error) {
	if output == "" {
		root, _, err := Home()
		if err != nil {
			return "", err
		}
		if output, err = ExportPath(root, file); err != nil {
			return "", err
		}
	} else {
		output = strings.TrimSuffix(strings.TrimSuffix(filepath.Clean(fsx.ExpandHome(output)), ".tar.gz"), ".tgz") + ".tar.gz"
	}
	if abs, err := filepath.Abs(output); err == nil {
		output = abs
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return "", err
	}
	return output, os.WriteFile(output, data, 0o600)
}

// Whether the path lies in a git checkout, true when that can't be told
func inCheckout(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return true
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	for at := abs; ; at = filepath.Dir(at) {
		if fsx.Lexists(filepath.Join(at, ".git")) {
			return true
		}
		if filepath.Dir(at) == at {
			return false
		}
	}
}

// Where an export goes when no output is named, under the file name the export gave it: the profile-exports folder of icebath's folder,
// else of the user's home or the temporary folder when that lies in a git checkout, refused when every one does, or the folder is a link
// or another user's
func ExportPath(root, file string) (string, error) {
	// The name is the server's to give and never a path
	if file = filepath.Base(file); file == "." || file == string(filepath.Separator) {
		return "", &ValueError{"The export gave its archive no file name. Name the file yourself, with -o on the command line."}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := ""
	for _, candidate := range []string{filepath.Join(root, "profile-exports"), filepath.Join(home, ".icebath-profile-exports"),
		filepath.Join(os.TempDir(), fmt.Sprintf("icebath-profile-exports-%d", os.Getuid()))} {
		if !inCheckout(candidate) {
			dir = candidate
			break
		}
	}
	if dir == "" {
		return "", &ValueError{"Every folder icebath would export to is inside a git repository, where the archive could be committed by mistake. " +
			"Name a file outside the repository yourself, with -o on the command line, such as -o ~/profile.tar.gz."}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if info, err := os.Lstat(dir); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return "", &ValueError{fmt.Sprintf("The export folder %s is a link, and icebath does not write an archive through a link. Name the file yourself, with -o on the command line.", dir)}
	}
	if owner, ok := ownerOf(dir); ok && owner != os.Getuid() {
		return "", &ValueError{fmt.Sprintf("The export folder %s belongs to another user, so icebath does not write an archive there. Name the file yourself, with -o on the command line.", dir)}
	}
	return filepath.Join(dir, file), nil
}
