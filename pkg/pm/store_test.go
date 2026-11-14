package pm

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"testing"
)

// Writes a gzipped tar of regular members with tarfile's default 0o644 mode
func stTarGz(t *testing.T, path string, files []member) string {
	t.Helper()
	var headers []*tar.Header
	data := map[string][]byte{}
	for _, f := range files {
		headers = append(headers, &tar.Header{Name: f.Name, Mode: 0o644, Typeflag: tar.TypeReg})
		data[f.Name] = f.Data
	}
	writeFile(t, path, makeTarHeaders(t, headers, data, true), 0o666)
	return path
}

// Digest of a tree as stored by test code
func stDigest(t *testing.T, root string) string {
	t.Helper()
	digest, err := TreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestMetadataDoesNotChangeExtractedLayoutOrPayloadDigest(t *testing.T) {
	tmp := t.TempDir()
	archive := stTarGz(t, filepath.Join(tmp, "package.tar.gz"), []member{
		{".DS_Store", []byte("outer")}, {"node/.DS_Store", []byte("inner")}, {"node/bin/node", []byte("runtime")},
	})
	tree := filepath.Join(tmp, "tree")
	if err := Extract(archive, tree); err != nil {
		t.Fatal(err)
	}
	if err := FlattenSingleDir(tree); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, filepath.Join(tree, "bin", "node"))); got != "runtime" {
		t.Fatalf("bin/node = %q", got)
	}
	if _, err := os.Stat(filepath.Join(tree, "node")); !os.IsNotExist(err) {
		t.Fatalf("wrapper still present: %v", err)
	}
	baseline := stDigest(t, tree)
	for _, name := range []string{".DS_Store", "._node", "Thumbs.db", "desktop.ini", ".localized"} {
		writeFile(t, filepath.Join(tree, name), []byte("desktop metadata"), 0o666)
	}
	if stDigest(t, tree) != baseline {
		t.Fatal("desktop metadata changed the payload digest")
	}
	writeFile(t, filepath.Join(tree, "bin", "node"), []byte("changed runtime"), 0o666)
	if stDigest(t, tree) == baseline {
		t.Fatal("payload change kept the digest")
	}
}

func TestMetadataNamedDirectoriesLinksAndExecutablesAreHashed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	tree := filepath.Join(t.TempDir(), "tree")
	directory := filepath.Join(tree, ".DS_Store")
	if err := os.MkdirAll(directory, 0o777); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(directory, "payload")
	writeFile(t, payload, []byte("first"), 0o666)
	first := stDigest(t, tree)
	writeFile(t, payload, []byte("second"), 0o666)
	if stDigest(t, tree) == first {
		t.Fatal("payload under a metadata named directory was skipped")
	}
	link := filepath.Join(tree, "._link")
	if err := os.Symlink(".DS_Store/payload", link); err != nil {
		t.Fatal(err)
	}
	first = stDigest(t, tree)
	os.Remove(link)
	if err := os.Symlink("missing-target", link); err != nil {
		t.Fatal(err)
	}
	if stDigest(t, tree) == first {
		t.Fatal("metadata named link text was skipped")
	}
	executable := filepath.Join(tree, "Thumbs.db")
	writeFile(t, executable, []byte("first"), 0o666)
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatal(err)
	}
	first = stDigest(t, tree)
	writeFile(t, executable, []byte("second"), 0o666)
	if stDigest(t, tree) == first {
		t.Fatal("metadata named executable was skipped")
	}
	wrapper := filepath.Join(tree, "node")
	if err := os.MkdirAll(filepath.Join(wrapper, "bin"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := FlattenSingleDir(tree); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(wrapper, "bin")); err != nil || !st.IsDir() {
		t.Fatalf("wrapper was hoisted: %v", err)
	}
}

func TestFactsRecordedWithMetadataInTheDigestStillMatch(t *testing.T) {
	entry := filepath.Join(t.TempDir(), "tools", "python-1.0")
	binary := filepath.Join(entry, "bin", "python3")
	writeFile(t, binary, []byte("runtime"), 0o666)
	writeFile(t, filepath.Join(entry, ".DS_Store"), []byte("desktop metadata"), 0o666)
	var rels []string
	if err := filepath.WalkDir(entry, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if st, serr := os.Stat(path); serr == nil && st.Mode().IsRegular() {
			rel, _ := filepath.Rel(entry, path)
			rels = append(rels, filepath.ToSlash(rel))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(rels)
	legacy := sha256.New()
	for _, rel := range rels {
		legacy.Write([]byte(rel))
		legacy.Write([]byte{0})
		legacy.Write(readFile(t, filepath.Join(entry, filepath.FromSlash(rel))))
	}
	recorded := hex.EncodeToString(legacy.Sum(nil))
	if ok, err := DigestMatches(entry, &recorded); err != nil || !ok {
		t.Fatalf("legacy digest with metadata did not verify: %v %v", ok, err)
	}
	writeFile(t, binary, []byte("changed runtime"), 0o666)
	if ok, err := DigestMatches(entry, &recorded); err != nil || ok {
		t.Fatalf("changed payload still verified: %v %v", ok, err)
	}
	if ok, err := DigestMatches(entry, nil); err != nil || ok {
		t.Fatalf("a missing recorded digest verified: %v %v", ok, err)
	}
}

func TestMetadataNamedPayloadCollisionRefusesBeforeAnyMove(t *testing.T) {
	tree := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(tree, "pkg", ".DS_Store"), 0o777); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "bin", "m", "z"} {
		writeFile(t, filepath.Join(tree, "pkg", name), []byte("payload"), 0o666)
	}
	writeFile(t, filepath.Join(tree, ".DS_Store"), []byte("desktop metadata"), 0o666)
	if err := FlattenSingleDir(tree); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(tree)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if !slices.Equal(names, []string{".DS_Store", "pkg"}) {
		t.Fatalf("top level = %v", names)
	}
	if inner, _ := os.ReadDir(filepath.Join(tree, "pkg")); len(inner) != 5 {
		t.Fatalf("pkg holds %d entries", len(inner))
	}
}
