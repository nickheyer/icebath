package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	ledgerBackupIDRe  = regexp.MustCompile(`^\p{Nd}{4}-\p{Nd}{2}-\p{Nd}{2}T\p{Nd}{2}-\p{Nd}{2}-\p{Nd}{2}Z(-\p{Nd}{2})?\n?\z`)
	ledgerArchiveTSRe = regexp.MustCompile(`^(.+)-\p{Nd}{14}\n?\z`)

	ledgerPackageRestoreActions = map[string]bool{"delete": true, "archive": true, "purge": true}
	ledgerValidActors           = map[string]bool{"curator": true, "agent": true, "user": true}
	ledgerNonPackageTops        = map[string]bool{".curator_backups": true, ".hub": true, ".archive": true, ".locks": true}
)

const (
	ledgerDefaultMaxBytes = 5 * 1024 * 1024
	ledgerBlobGCGrace     = 3600 * time.Second
	ledgerTrimLowWater    = 0.8
)

// Transient local artifact directory names never swept into a ledger snapshot or a curator tarball
var TransientDirs = map[string]bool{
	".venv": true, "venv": true, "env": true, ".env": true,
	"node_modules": true, "__pycache__": true,
	".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true,
	".git": true,
}

// Ledger write a skill mutation records, Before nil with BeforeCaptured false captures from BeforeRoot
type LedgerMutation struct {
	BeforeRoot     string
	Before         []map[string]string
	BeforeCaptured bool
	AfterRoot      string
	Actor          string
	Evidence       *pyjson.Object
}

// Snapshot, append and maintenance steps of the ledger, swapped by tests to observe, fail and pause them
var ledgerSteps struct {
	compact  func(s *Scope, ctx context.Context) (int, int, int, error)
	trim     func(s *Scope, ctx context.Context, maxBytes int64) (int, error)
	rewrite  func(ctx context.Context, path string, lines [][]byte, op string) ([]byte, error)
	snapshot func(s *Scope, root string, completePackage bool) ([]any, error)
	append   func(s *Scope, ctx context.Context, action string, skill any, before, after []any, actor string, evidence *pyjson.Object) (string, bool)
}

func init() {
	ledgerSteps.compact = (*Scope).LedgerCompact
	ledgerSteps.trim = (*Scope).ledgerTrimOldest
	ledgerSteps.rewrite = ledgerRewrite
	ledgerSteps.snapshot = (*Scope).ledgerSnapshot
	ledgerSteps.append = (*Scope).ledgerAppend
}

type ledgerActorKey struct{}

// Context binding an explicit ledger actor, an empty actor clears the override
func WithLedgerActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, ledgerActorKey{}, actor)
}

// Explicit actor override, else curator for background review provenance, else agent
func (s *Scope) LedgerDeriveActor(ctx context.Context) string {
	if a, _ := ctx.Value(ledgerActorKey{}).(string); ledgerValidActors[a] {
		return a
	}
	if IsBackgroundReview(s.Env) {
		return "curator"
	}
	return "agent"
}

func (s *Scope) ledgerSkillsDir() string { return filepath.Join(s.Home, "skills") }

// Path of the JSONL skill mutation ledger
func (s *Scope) LedgerPath() string {
	return filepath.Join(s.ledgerSkillsDir(), ".curator_ledger.jsonl")
}

// Content addressed blob store the ledger manifests point into
func (s *Scope) LedgerBlobsDir() string { return filepath.Join(s.Home, ".curator_backups", "blobs") }

// Cross process ledger lock held across an append and its maintenance sweep
func (s *Scope) ledgerLock(ctx context.Context) (context.Context, func(), error) {
	return SkillFileLock(ctx, filepath.Join(s.ledgerSkillsDir(), ".locks", "ledger.lock"))
}

// One skills config value with dict get semantics, the default only when the key is absent
func (s *Scope) ledgerCfg(key string, fallback any) any {
	if v, ok := config.CfgGet(map[string]any(s.tree()), "skills", key); ok {
		return v
	}
	return fallback
}

// Truthiness of skills.ledger, on by default
func (s *Scope) LedgerEnabled() bool { return pyrepr.Truthy(s.ledgerCfg("ledger", true)) }

func (s *Scope) ledgerMaxBytes() (*big.Int, error) {
	return pyint.Of(s.ledgerCfg("ledger_max_bytes", ledgerDefaultMaxBytes))
}

// Posix path of p under root after normpath of both, false when outside
func ledgerRelPosix(p, root string) (string, bool) {
	return pypath.PathRelativeTo(pypath.Normpath(p), pypath.Normpath(root))
}

func ledgerWithin(root, p string) bool {
	_, ok := ledgerRelPosix(p, root)
	return ok
}

// Random lowercase hex of n bytes, the leading digits of a uuid4 hex
func ledgerRandomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Stores data keyed by its sha256 and returns the digest, an existing blob left alone
func (s *Scope) ledgerStoreBlob(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	dest := filepath.Join(s.LedgerBlobsDir(), digest)
	if _, err := os.Stat(dest); err == nil {
		return digest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return "", err
	}
	tmp := filepath.Join(filepath.Dir(dest), ".tmp-"+ledgerRandomHex(4)+"-"+digest)
	if err := os.WriteFile(tmp, data, 0o666); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return digest, nil
}

// Blob content, false when the hash is malformed or the blob is missing or unreadable
func (s *Scope) LedgerReadBlob(sha string) ([]byte, bool) {
	if sha == "" || strings.Trim(sha, "0123456789abcdef") != "" {
		return nil, false
	}
	p := filepath.Join(s.LedgerBlobsDir(), sha)
	if _, err := os.Stat(p); err != nil {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	return data, true
}

func newLedgerItem(path, sha string) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("path", path)
	o.Set("sha256", sha)
	return o
}

// Python item.get(key, default) on a manifest item, AttributeError text for a non dict
func ledgerItemGet(item any, key string, fallback any) (any, error) {
	o, ok := item.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(item))
	}
	if v, ok := o.Get(key); ok {
		return v, nil
	}
	return fallback, nil
}

// Python item[key] on a manifest item, KeyError or TypeError text when it fails
func ledgerItemKey(item any, key string) (any, error) {
	o, ok := item.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object is not subscriptable", pyrepr.TypeName(item))
	}
	if v, ok := o.Get(key); ok {
		return v, nil
	}
	return nil, fmt.Errorf("'%s'", key)
}

// Elements Python iteration of value or [] yields, dict keys and string characters included
func ledgerIter(v any) ([]any, error) {
	if !pyrepr.Truthy(v) {
		return []any{}, nil
	}
	switch x := v.(type) {
	case []any:
		return x, nil
	case *pyjson.Object:
		out := make([]any, len(x.Keys))
		for i, k := range x.Keys {
			out[i] = k
		}
		return out, nil
	case string:
		var out []any
		for _, r := range pystr.Runes(x) {
			out = append(out, pystr.FromRunes([]rune{r}))
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(v))
}

// Python (a or []) + (b or []) over two ledger manifests
func ledgerConcat(a, b any) ([]any, error) {
	if !pyrepr.Truthy(a) {
		a = []any{}
	}
	if !pyrepr.Truthy(b) {
		b = []any{}
	}
	la, okA := a.([]any)
	lb, okB := b.([]any)
	sa, strA := a.(string)
	sb, strB := b.(string)
	switch {
	case okA && okB:
		return append(append([]any{}, la...), lb...), nil
	case okA:
		return nil, fmt.Errorf("can only concatenate list (not \"%s\") to list", pyrepr.TypeName(b))
	case strA && strB:
		return ledgerIter(sa + sb)
	case strA:
		return nil, fmt.Errorf("can only concatenate str (not \"%s\") to str", pyrepr.TypeName(b))
	}
	return nil, fmt.Errorf("unsupported operand type(s) for +: '%s' and '%s'", pyrepr.TypeName(a), pyrepr.TypeName(b))
}

func ledgerObjects(items []*pyjson.Object) []any {
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = it
	}
	return out
}

func ledgerTyped(items []any) []*pyjson.Object {
	out := make([]*pyjson.Object, 0, len(items))
	for _, it := range items {
		if o, ok := it.(*pyjson.Object); ok {
			out = append(out, o)
		}
	}
	return out
}

// Child path joined the way pathlib joins one name onto a normalized directory
func ledgerChild(dir, name string) string {
	if strings.HasSuffix(dir, string(os.PathSeparator)) || strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + string(os.PathSeparator) + name
}

// Regular files under root in pathlib sort order, symlinked and transient directories not entered
func ledgerWalkFiles(root string) []string {
	type file struct {
		path  string
		parts []string
	}
	var found []file
	var walk func(dir string, parts []string)
	walk = func(dir string, parts []string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			p := ledgerChild(dir, e.Name())
			sub := append(append([]string(nil), parts...), e.Name())
			if e.IsDir() {
				if !TransientDirs[e.Name()] {
					walk(p, sub)
				}
				continue
			}
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				found = append(found, file{p, sub})
			}
		}
	}
	walk(root, nil)
	norm := func(s string) string {
		if runtime.GOOS == "windows" {
			return strings.ToLower(s)
		}
		return s
	}
	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i].parts, found[j].parts
		for k := 0; k < len(a) && k < len(b); k++ {
			if x, y := norm(a[k]), norm(b[k]); x != y {
				return x < y
			}
		}
		return len(a) < len(b)
	})
	out := make([]string, len(found))
	for i, f := range found {
		out[i] = f.path
	}
	return out
}

// Path and sha256 of every file under root stored as blobs, empty root or missing path gives none
func (s *Scope) ledgerSnapshot(root string, completePackage bool) ([]any, error) {
	if root == "" {
		return []any{}, nil
	}
	root = pypath.PathStr(root)
	var files []string
	if st, err := os.Stat(root); err == nil {
		if st.Mode().IsRegular() {
			files = []string{root}
		} else if st.IsDir() {
			files = ledgerWalkFiles(root)
		}
	}
	out := make([]any, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		sha, err := s.ledgerStoreBlob(data)
		if err != nil {
			return nil, err
		}
		out = append(out, newLedgerItem(f, sha))
	}
	if completePackage {
		return s.ledgerFill(root, out, "")
	}
	return out, nil
}

// Snapshot of root, unioned with the newest curator tarball's package files when completePackage is set
func (s *Scope) LedgerSnapshotPaths(root string, completePackage bool) ([]*pyjson.Object, error) {
	items, err := ledgerSteps.snapshot(s, root, completePackage)
	return ledgerTyped(items), err
}

// Package path of a skill dir under skills, empty outside it or under metadata roots
func (s *Scope) ledgerPackageRel(root string) string {
	rel, _ := ledgerRelPosix(root, s.ledgerSkillsDir())
	posix := strings.Trim(rel, "/")
	if posix == "" || ledgerNonPackageTops[strings.SplitN(posix, "/", 2)[0]] {
		return ""
	}
	return posix
}

func ledgerStripArchiveTimestamp(name string) string {
	if m := ledgerArchiveTSRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

// Parent directories of every SKILL.md item in a manifest
func ledgerSkillMDParents(items []any) ([]string, error) {
	var out []string
	for _, it := range items {
		v, err := ledgerItemGet(it, "path", "")
		if err != nil {
			return nil, err
		}
		p := pypath.PathStr(pyrepr.Str(v))
		if pypath.PathName(p) == "SKILL.md" {
			out = append(out, pypath.PathParent(p))
		}
	}
	return out, nil
}

// Tarball member prefixes of a skill package from its live dir, before state SKILL.md parents and name
func (s *Scope) ledgerPackagePrefixes(root, skill string, before []any) ([]string, error) {
	var cands []string
	if root != "" {
		cands = append(cands, s.ledgerPackageRel(root))
	}
	parents, err := ledgerSkillMDParents(before)
	if err != nil {
		return nil, err
	}
	for _, p := range parents {
		cands = append(cands, s.ledgerPackageRel(p))
	}
	cands = append(cands, skill)
	if skill != "" {
		cands = append(cands, ledgerStripArchiveTimestamp(skill))
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range cands {
		if c = strings.Trim(c, "/"); c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

// Tarball member prefixes of a skill package, root and skill empty for none
func (s *Scope) LedgerPackagePrefixes(root, skill string, before []*pyjson.Object) ([]string, error) {
	return s.ledgerPackagePrefixes(root, skill, ledgerObjects(before))
}

// Files under prefixes in the newest curator skills tarball in member order, traversal members rejected
func (s *Scope) ledgerReadBackupPackage(prefixes []string) ([]string, map[string][]byte) {
	backups := filepath.Join(s.ledgerSkillsDir(), ".curator_backups")
	if len(prefixes) == 0 {
		return nil, nil
	}
	if st, err := os.Stat(backups); err != nil || !st.IsDir() {
		return nil, nil
	}
	children, err := os.ReadDir(backups)
	if err != nil {
		return nil, nil
	}
	best := ""
	for _, c := range children {
		child := filepath.Join(backups, c.Name())
		st, err := os.Stat(child)
		if err != nil || !st.IsDir() || !ledgerBackupIDRe.MatchString(c.Name()) {
			continue
		}
		if ts, err := os.Stat(filepath.Join(child, "skills.tar.gz")); err != nil || !ts.Mode().IsRegular() {
			continue
		}
		if best == "" || c.Name() > best {
			best = c.Name()
		}
	}
	if best == "" {
		return nil, nil
	}
	exact := map[string]bool{}
	var prefixed []string
	for _, p := range prefixes {
		exact[p] = true
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		prefixed = append(prefixed, p)
	}
	keys, files, err := ledgerReadTarball(filepath.Join(backups, best, "skills.tar.gz"), exact, prefixed)
	if err != nil {
		s.logger().Debug(fmt.Sprintf("skill_ledger: could not read curator backup package: %s", pyOSErrorText(err)))
		return nil, nil
	}
	return keys, files
}

// Regular members of a gzip tarball whose cleaned name matches exact or a prefix
func ledgerReadTarball(archive string, exact map[string]bool, prefixed []string) ([]string, map[string][]byte, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, err
	}
	tr := tar.NewReader(gz)
	var keys []string
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		switch hdr.Typeflag {
		case tar.TypeReg, '\x00', tar.TypeCont, tar.TypeGNUSparse:
		default:
			continue
		}
		name := strings.TrimLeft(strings.ReplaceAll(hdr.Name, `\`, "/"), "./")
		if name == "" || strings.HasPrefix(name, "/") {
			continue
		}
		_, parts := pypath.PathParts(name)
		traversal := false
		for _, p := range parts {
			traversal = traversal || p == ".."
		}
		matched := exact[name]
		for _, p := range prefixed {
			matched = matched || strings.HasPrefix(name, p)
		}
		if traversal || !matched {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, err
		}
		if _, dup := files[name]; !dup {
			keys = append(keys, name)
		}
		files[name] = data
	}
	return keys, files, nil
}

// Unions absent skill package files from the newest curator tarball into a manifest, disk entries win
func (s *Scope) ledgerFill(root string, existing []any, skill string) ([]any, error) {
	out := append([]any{}, existing...)
	prefixes, err := s.ledgerPackagePrefixes(root, skill, out)
	if err != nil {
		return nil, err
	}
	if len(prefixes) == 0 {
		return out, nil
	}
	keys, extra := s.ledgerReadBackupPackage(prefixes)
	if len(keys) == 0 {
		return out, nil
	}
	skills := s.ledgerSkillsDir()
	destRoot := ""
	pkgNames := map[string]bool{}
	if root != "" {
		destRoot = pypath.PathStr(root)
		name := pypath.PathName(destRoot)
		pkgNames[name] = true
		pkgNames[ledgerStripArchiveTimestamp(name)] = true
	}
	have := map[string]bool{}
	for _, it := range out {
		v, err := ledgerItemGet(it, "path", "")
		if err != nil {
			return nil, err
		}
		if rel, ok := ledgerRelPosix(pyrepr.Str(v), skills); ok {
			have[rel] = true
		}
	}
	for _, rel := range keys {
		parts := strings.Split(rel, "/")
		if destRoot != "" && pkgNames[parts[0]] {
			parts = parts[1:]
		}
		if len(parts) == 0 {
			continue
		}
		base := skills
		if destRoot != "" {
			base = destRoot
		}
		dest := pypath.PathJoin(base, parts...)
		if !ledgerWithin(skills, dest) || !ledgerWithin(s.Home, dest) {
			continue
		}
		relKey, ok := ledgerRelPosix(dest, skills)
		if !ok || have[relKey] {
			continue
		}
		sha, err := s.ledgerStoreBlob(extra[rel])
		if err != nil {
			s.logger().Debug(fmt.Sprintf("skill_ledger: backup blob store failed for %s: %s", rel, pyOSErrorText(err)))
			continue
		}
		out = append(out, newLedgerItem(dest, sha))
		have[relKey] = true
	}
	return out, nil
}

// Manifest unioned with absent package files from the newest curator tarball, root and skill empty for none
func (s *Scope) LedgerFillSnapshotFromCuratorBackup(root string, existing []*pyjson.Object, skill string) ([]*pyjson.Object, error) {
	items, err := s.ledgerFill(root, ledgerObjects(existing), skill)
	return ledgerTyped(items), err
}

// Drops paths whose hash is identical on both sides, one sided paths kept
func ledgerDelta(before, after []any) ([]any, []any, error) {
	pathOf := func(it any) (string, any, error) {
		p, err := ledgerItemGet(it, "path", nil)
		if err != nil {
			return "", nil, err
		}
		sha, _ := ledgerItemGet(it, "sha256", nil)
		return pyrepr.Str(p), sha, nil
	}
	bSha, aSha := map[string]any{}, map[string]any{}
	var bOrder []string
	for _, it := range before {
		p, sha, err := pathOf(it)
		if err != nil {
			return nil, nil, err
		}
		if _, ok := bSha[p]; !ok {
			bOrder = append(bOrder, p)
		}
		bSha[p] = sha
	}
	for _, it := range after {
		p, sha, err := pathOf(it)
		if err != nil {
			return nil, nil, err
		}
		aSha[p] = sha
	}
	same := map[string]bool{}
	for _, p := range bOrder {
		if pyrepr.Equal(aSha[p], bSha[p]) {
			same[p] = true
		}
	}
	keep := func(items []any) []any {
		out := []any{}
		for _, it := range items {
			if p, _, _ := pathOf(it); !same[p] {
				out = append(out, it)
			}
		}
		return out
	}
	return keep(before), keep(after), nil
}

// Python exception text of a ledger failure
func ledgerErrText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pyOSErrorText(err)
	}
	return err.Error()
}

// Appends one entry and returns its id, false when the ledger is off or the write failed
func (s *Scope) ledgerAppend(ctx context.Context, action string, skill any, before, after []any, actor string, evidence *pyjson.Object) (string, bool) {
	if !s.LedgerEnabled() {
		return "", false
	}
	id, err := s.ledgerAppendEntry(ctx, action, skill, before, after, actor, evidence)
	if err != nil {
		s.logger().Warn(fmt.Sprintf("skill_ledger: failed to append entry (%s) — mutation unaffected", ledgerErrText(err)))
		return "", false
	}
	return id, true
}

func (s *Scope) ledgerAppendEntry(ctx context.Context, action string, skill any, before, after []any, actor string, evidence *pyjson.Object) (string, error) {
	if before == nil {
		before = []any{}
	}
	if after == nil {
		after = []any{}
	}
	if action != "pre-rollback" {
		var err error
		if before, after, err = ledgerDelta(before, after); err != nil {
			return "", err
		}
	}
	if !ledgerValidActors[actor] {
		actor = s.LedgerDeriveActor(ctx)
	}
	if evidence == nil {
		evidence = pyjson.NewObject()
	}
	id := ledgerRandomHex(6)
	entry := pyjson.NewObject()
	entry.Set("id", id)
	entry.Set("ts", nowISO())
	entry.Set("actor", actor)
	entry.Set("action", action)
	entry.Set("skill", skill)
	entry.Set("evidence", evidence)
	entry.Set("before", before)
	entry.Set("after", after)
	line, err := pyjson.Dumps(entry, pyjson.EnsureASCII(false))
	if err != nil {
		return "", err
	}
	path := s.LedgerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return "", err
	}
	lctx, release, err := s.ledgerLock(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	terminator := "\n"
	if runtime.GOOS == "windows" {
		terminator = "\r\n"
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return "", err
	}
	_, werr := f.WriteString(line + terminator)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", werr
	}
	if err := s.ledgerMaintainSize(lctx); err != nil {
		return "", err
	}
	return id, nil
}

// Appends one entry over typed manifests, false when the ledger is off or the write failed
func (s *Scope) LedgerAppendEntry(ctx context.Context, action, skill string, before, after []*pyjson.Object, actor string, evidence *pyjson.Object) (string, bool) {
	var b, a []any
	if before != nil {
		b = ledgerObjects(before)
	}
	if after != nil {
		a = ledgerObjects(after)
	}
	return ledgerSteps.append(s, ctx, action, skill, b, a, actor, evidence)
}

// Ledger bytes with a UTF-8 BOM removed, nil after a warning when unreadable or not UTF-8
func (s *Scope) ledgerRead(what string, quietMissing bool) []byte {
	raw, err := os.ReadFile(s.LedgerPath())
	if err != nil {
		if !quietMissing || !errors.Is(err, fs.ErrNotExist) {
			s.logger().Warn(fmt.Sprintf("skill_ledger: ledger unreadable (%s); %s", pyOSErrorText(err), what))
		}
		return nil
	}
	raw = bytes.TrimPrefix(raw, []byte("\xEF\xBB\xBF"))
	if i, ok := pystr.ValidIndex(raw); !ok {
		s.logger().Warn(fmt.Sprintf("skill_ledger: ledger unreadable (%s); %s", pystr.UTF8DecodeError(raw, i).Error(), what))
		return nil
	}
	if raw == nil {
		raw = []byte{}
	}
	return raw
}

// Bounds the ledger at skills.ledger_max_bytes through compaction, then oldest first trimming and blob GC
func (s *Scope) ledgerMaintainSize(ctx context.Context) error {
	maxBytes, err := s.ledgerMaxBytes()
	if err != nil {
		return err
	}
	if maxBytes.Sign() <= 0 {
		return nil
	}
	if err := s.ledgerSweep(ctx, maxBytes); err != nil {
		s.logger().Warn(fmt.Sprintf("skill_ledger: maintenance sweep failed (%s) — ledger left as-is", ledgerErrText(err)))
	}
	return nil
}

func (s *Scope) ledgerSweep(ctx context.Context, maxBytes *big.Int) error {
	st, err := os.Stat(s.LedgerPath())
	if err != nil {
		return err
	}
	if big.NewInt(st.Size()).Cmp(maxBytes) <= 0 {
		return nil
	}
	_, _, sizeAfter, err := ledgerSteps.compact(s, ctx)
	if err != nil {
		return err
	}
	lowWater := int64(float64(maxBytes.Int64()) * ledgerTrimLowWater)
	dropped := 0
	if int64(sizeAfter) > lowWater {
		if dropped, err = ledgerSteps.trim(s, ctx, lowWater); err != nil {
			return err
		}
	}
	if dropped != 0 {
		_, _, err = s.LedgerGCBlobs(ctx)
	}
	return err
}

// Atomically replaces the ledger with lines through a sibling temp file and returns the bytes written
func ledgerRewrite(_ context.Context, path string, lines [][]byte, op string) ([]byte, error) {
	var data []byte
	if len(lines) > 0 {
		data = append(bytes.Join(lines, []byte("\n")), '\n')
	}
	tmp := path + "." + op + ".tmp"
	if err := os.WriteFile(tmp, data, 0o666); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	return data, nil
}

// Drops the oldest physical lines until the ledger fits maxBytes, the newest line always kept
func (s *Scope) ledgerTrimOldest(ctx context.Context, maxBytes int64) (int, error) {
	lctx, release, err := s.ledgerLock(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	raw := s.ledgerRead("trim skipped", false)
	if raw == nil {
		return 0, nil
	}
	lines := bytes.Split(raw, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	var size int64
	start := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		addition := int64(len(lines[i]) + 1)
		if start < len(lines) && size+addition > maxBytes {
			break
		}
		start = i
		size += addition
	}
	if start == 0 {
		return 0, nil
	}
	if _, err := ledgerSteps.rewrite(lctx, s.LedgerPath(), lines[start:], "trim"); err != nil {
		return 0, err
	}
	return start, nil
}

// Rewrites the ledger with unchanged paths dropped from every entry, returning entries and sizes before and after
func (s *Scope) LedgerCompact(ctx context.Context) (int, int, int, error) {
	lctx, release, err := s.ledgerLock(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	defer release()
	raw := s.ledgerRead("compaction skipped", false)
	if raw == nil {
		return 0, 0, 0, nil
	}
	var out []string
	kept := 0
	for _, line := range pystr.Splitlines(string(raw), false) {
		if pystr.Strip(line) == "" {
			continue
		}
		row, err := pyjson.Loads(line)
		if err != nil {
			var de *pyjson.DecodeError
			if errors.As(err, &de) {
				out = append(out, line)
				continue
			}
			return 0, 0, 0, err
		}
		if obj, ok := row.(*pyjson.Object); ok && !ledgerIsPreRollback(obj) {
			bv, _ := obj.Get("before")
			av, _ := obj.Get("after")
			b, err := ledgerIter(bv)
			if err != nil {
				return 0, 0, 0, err
			}
			a, err := ledgerIter(av)
			if err != nil {
				return 0, 0, 0, err
			}
			if b, a, err = ledgerDelta(b, a); err != nil {
				return 0, 0, 0, err
			}
			obj.Set("before", b)
			obj.Set("after", a)
			if line, err = pyjson.Dumps(obj, pyjson.EnsureASCII(false)); err != nil {
				return 0, 0, 0, err
			}
		}
		out = append(out, line)
		kept++
	}
	lines := make([][]byte, len(out))
	for i, l := range out {
		lines[i] = []byte(l)
	}
	var joined []byte
	if len(lines) > 0 {
		joined = append(bytes.Join(lines, []byte("\n")), '\n')
	}
	if bytes.Equal(joined, raw) {
		return kept, len(raw), len(raw), nil
	}
	data, err := ledgerSteps.rewrite(lctx, s.LedgerPath(), lines, "compact")
	if err != nil {
		return 0, 0, 0, err
	}
	return kept, len(raw), len(data), nil
}

func ledgerIsPreRollback(row *pyjson.Object) bool {
	v, _ := row.Get("action")
	a, ok := v.(string)
	return ok && a == "pre-rollback"
}

// Deletes blobs no entry references and older than the grace window, returning count and bytes freed
func (s *Scope) LedgerGCBlobs(ctx context.Context) (int, int64, error) {
	_, release, err := s.ledgerLock(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer release()
	blobs := s.LedgerBlobsDir()
	if st, err := os.Stat(blobs); err != nil || !st.IsDir() {
		return 0, 0, nil
	}
	raw := s.ledgerRead("blob GC skipped", false)
	if raw == nil {
		return 0, 0, nil
	}
	referenced := map[string]bool{}
	for _, line := range pystr.Splitlines(string(raw), false) {
		if pystr.Strip(line) == "" {
			continue
		}
		row, err := pyjson.Loads(line)
		if err != nil {
			var de *pyjson.DecodeError
			if !errors.As(err, &de) {
				return 0, 0, err
			}
			row = nil
		}
		obj, ok := row.(*pyjson.Object)
		if !ok {
			s.logger().Warn("skill_ledger: malformed ledger line; blob GC skipped")
			return 0, 0, nil
		}
		bv, _ := obj.Get("before")
		av, _ := obj.Get("after")
		items, err := ledgerConcat(bv, av)
		if err != nil {
			return 0, 0, err
		}
		for _, it := range items {
			v, err := ledgerItemGet(it, "sha256", "")
			if err != nil {
				return 0, 0, err
			}
			referenced[pyrepr.Str(v)] = true
		}
	}
	entries, err := os.ReadDir(blobs)
	if err != nil {
		return 0, 0, err
	}
	deleted, freed := 0, int64(0)
	freshAfter := time.Now().Add(-ledgerBlobGCGrace)
	for _, e := range entries {
		p := filepath.Join(blobs, e.Name())
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() || referenced[e.Name()] || st.ModTime().After(freshAfter) {
			continue
		}
		if err := os.Remove(p); err != nil {
			continue
		}
		deleted++
		freed += st.Size()
	}
	return deleted, freed, nil
}

// Typed manifest items as Python dicts with path and sha256 first
func ledgerFromMaps(items []map[string]string) []any {
	out := make([]any, len(items))
	for i, m := range items {
		o := pyjson.NewObject()
		for _, k := range []string{"path", "sha256"} {
			if v, ok := m[k]; ok {
				o.Set(k, v)
			}
		}
		var rest []string
		for k := range m {
			if k != "path" && k != "sha256" {
				rest = append(rest, k)
			}
		}
		sort.Strings(rest)
		for _, k := range rest {
			o.Set(k, m[k])
		}
		out[i] = o
	}
	return out
}

func ledgerToMaps(items []any) []map[string]string {
	out := make([]map[string]string, 0, len(items))
	for _, it := range items {
		m := map[string]string{}
		if o, ok := it.(*pyjson.Object); ok {
			for k, v := range o.Vals {
				m[k] = pyrepr.Str(v)
			}
		}
		out = append(out, m)
	}
	return out
}

// Records a mutation with its after state from AfterRoot, never failing the mutation itself
func (s *Scope) LedgerRecordMutation(ctx context.Context, action, skill string, m LedgerMutation) (string, bool) {
	if !s.LedgerEnabled() {
		return "", false
	}
	id, ok, err := s.ledgerRecord(ctx, action, skill, m)
	if err != nil {
		s.logger().Warn(fmt.Sprintf("skill_ledger: record_mutation failed (%s) — mutation unaffected", ledgerErrText(err)))
		return "", false
	}
	return id, ok
}

func (s *Scope) ledgerRecord(ctx context.Context, action, skill string, m LedgerMutation) (string, bool, error) {
	complete := ledgerPackageRestoreActions[action]
	var before []any
	var err error
	switch {
	case !m.BeforeCaptured:
		before, err = ledgerSteps.snapshot(s, m.BeforeRoot, complete)
	case complete:
		before, err = s.ledgerFill(m.BeforeRoot, ledgerFromMaps(m.Before), skill)
	default:
		before = ledgerFromMaps(m.Before)
	}
	if err != nil {
		return "", false, err
	}
	after, err := ledgerSteps.snapshot(s, m.AfterRoot, false)
	if err != nil {
		return "", false, err
	}
	id, ok := ledgerSteps.append(s, ctx, action, skill, before, after, m.Actor, m.Evidence)
	return id, ok, nil
}

// Pre mutation capture of root, nil when the ledger is off or the capture failed
func (s *Scope) LedgerCaptureBefore(ctx context.Context, root string, completePackage bool, skill string) []map[string]string {
	if !s.LedgerEnabled() {
		return nil
	}
	captured, err := ledgerSteps.snapshot(s, root, false)
	if err == nil && completePackage {
		captured, err = s.ledgerFill(root, captured, skill)
	}
	if err != nil {
		s.logger().Warn(fmt.Sprintf("skill_ledger: before-capture failed (%s) — mutation unaffected", ledgerErrText(err)))
		return nil
	}
	return ledgerToMaps(captured)
}

// Ledger rows newest first filtered by skill when set, limit below zero for all
func (s *Scope) LedgerListEntries(skill string, limit int) ([]*pyjson.Object, error) {
	raw := s.ledgerRead("listing empty", true)
	rows := []*pyjson.Object{}
	if raw == nil {
		return rows, nil
	}
	for _, line := range pystr.Splitlines(string(raw), false) {
		if pystr.Strip(line) == "" {
			continue
		}
		row, err := pyjson.Loads(line)
		if err != nil {
			var de *pyjson.DecodeError
			if errors.As(err, &de) {
				continue
			}
			return nil, err
		}
		obj, ok := row.(*pyjson.Object)
		if !ok {
			continue
		}
		if v, _ := obj.Get("skill"); skill == "" || pyrepr.Equal(v, skill) {
			rows = append(rows, obj)
		}
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	if limit >= 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	return rows, nil
}

// Ledger row with the given id, nil when absent or the id is empty
func (s *Scope) LedgerGetEntry(entryID string) (*pyjson.Object, error) {
	if entryID == "" {
		return nil, nil
	}
	rows, err := s.LedgerListEntries("", -1)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if v, _ := r.Get("id"); pyrepr.Equal(v, entryID) {
			return r, nil
		}
	}
	return nil, nil
}

// Refusal text when any manifest path sits outside the home, empty when all are inside
func (s *Scope) ledgerValidatePaths(sections ...any) (string, error) {
	h := pypath.PathStr(s.Home)
	for _, sec := range sections {
		items, err := ledgerIter(sec)
		if err != nil {
			return "", err
		}
		for _, it := range items {
			v, err := ledgerItemGet(it, "path", "")
			if err != nil {
				return "", err
			}
			p := pypath.PathStr(pyrepr.Str(v))
			if !ledgerWithin(h, p) {
				return fmt.Sprintf("entry references a path outside %s: %s", h, p), nil
			}
		}
	}
	return "", nil
}

// Restores the before state of an entry after appending an undoable pre-rollback safety entry
func (s *Scope) LedgerRollbackEntry(ctx context.Context, entryID string) (bool, string, error) {
	entry, err := s.LedgerGetEntry(entryID)
	if err != nil {
		return false, "", err
	}
	if entry == nil {
		return false, fmt.Sprintf("no ledger entry with id '%s'", entryID), nil
	}
	bv, _ := entry.Get("before")
	av, _ := entry.Get("after")
	if msg, err := s.ledgerValidatePaths(bv, av); err != nil {
		return false, "", err
	} else if msg != "" {
		return false, "refusing rollback: " + msg, nil
	}
	before, err := ledgerIter(bv)
	if err != nil {
		return false, "", err
	}
	after, err := ledgerIter(av)
	if err != nil {
		return false, "", err
	}
	actionV, _ := entry.Get("action")
	switch actionV.(type) {
	case []any, *pyjson.Object:
		return false, "", fmt.Errorf("unhashable type: '%s'", pyrepr.TypeName(actionV))
	}
	if a, ok := actionV.(string); ok && ledgerPackageRestoreActions[a] {
		parents, err := ledgerSkillMDParents(before)
		if err != nil {
			return false, "", err
		}
		root := ""
		if len(parents) > 0 {
			root = parents[0]
		}
		skillV, _ := entry.Get("skill")
		skill := ""
		if pyrepr.Truthy(skillV) {
			skill = pyrepr.Str(skillV)
		}
		if before, err = s.ledgerFill(root, before, skill); err != nil {
			return false, "", err
		}
		if msg, err := s.ledgerValidatePaths(before, after); err != nil {
			return false, "", err
		} else if msg != "" {
			return false, "refusing rollback: " + msg, nil
		}
	}
	for _, it := range before {
		shaV, err := ledgerItemGet(it, "sha256", "")
		if err != nil {
			return false, "", err
		}
		if _, ok := s.LedgerReadBlob(pyrepr.Str(shaV)); !ok {
			sha, _ := ledgerItemGet(it, "sha256", nil)
			path, _ := ledgerItemGet(it, "path", nil)
			return false, fmt.Sprintf("missing blob %s for %s; rollback aborted, nothing was changed", pyrepr.Str(sha), pyrepr.Str(path)), nil
		}
	}
	touched := map[string]bool{}
	for _, it := range append(append([]any{}, before...), after...) {
		v, err := ledgerItemGet(it, "path", nil)
		if err != nil {
			return false, "", err
		}
		if pyrepr.Truthy(v) {
			touched[pyrepr.Str(v)] = true
		}
	}
	paths := make([]string, 0, len(touched))
	for p := range touched {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	safety := []any{}
	for _, p := range paths {
		if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(p)
		if err == nil {
			var sha string
			if sha, err = s.ledgerStoreBlob(data); err == nil {
				safety = append(safety, newLedgerItem(p, sha))
				continue
			}
		}
		return false, fmt.Sprintf("pre-rollback safety capture failed (%s); rollback aborted and current skills were not changed", ledgerErrText(err)), nil
	}
	skillV, ok := entry.Get("skill")
	if !ok {
		skillV = "?"
	}
	ev := pyjson.NewObject()
	ev.Set("rollback_target", entryID)
	safetyID, ok := ledgerSteps.append(s, ctx, "pre-rollback", skillV, safety, safety, "", ev)
	if !ok {
		return false, "pre-rollback safety capture failed (ledger disabled or unwritable); rollback aborted and current skills were not changed", nil
	}
	beforePaths := map[string]bool{}
	for _, it := range before {
		v, err := ledgerItemKey(it, "path")
		if err != nil {
			return false, "", err
		}
		beforePaths[pyrepr.Str(v)] = true
	}
	for _, it := range before {
		pv, err := ledgerItemKey(it, "path")
		if err != nil {
			return false, "", err
		}
		fp := pypath.PathStr(pyrepr.Str(pv))
		if err := os.MkdirAll(pypath.PathParent(fp), 0o777); err != nil {
			return false, "", err
		}
		sv, err := ledgerItemKey(it, "sha256")
		if err != nil {
			return false, "", err
		}
		data, ok := s.LedgerReadBlob(pyrepr.Str(sv))
		if !ok {
			return false, "", errors.New("memoryview: a bytes-like object is required, not 'NoneType'")
		}
		if err := os.WriteFile(fp, data, 0o666); err != nil {
			return false, "", err
		}
	}
	restored, removed := len(before), 0
	for _, it := range after {
		v, err := ledgerItemGet(it, "path", "")
		if err != nil {
			return false, "", err
		}
		p := pyrepr.Str(v)
		if p == "" || beforePaths[p] {
			continue
		}
		if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(p); err != nil {
			s.logger().Warn(fmt.Sprintf("skill_ledger: could not remove %s during rollback: %s", p, pyOSErrorText(err)))
			continue
		}
		removed++
	}
	done := pyjson.NewObject()
	done.Set("rollback_target", entryID)
	done.Set("restored", restored)
	done.Set("removed", removed)
	ledgerSteps.append(s, ctx, "rollback", skillV, safety, before, "", done)
	action, _ := entry.Get("action")
	skillShown, _ := entry.Get("skill")
	return true, fmt.Sprintf("rolled back entry %s (%s on '%s'): %d file(s) restored, %d removed. Safety entry %s captured the pre-rollback state.",
		entryID, pyrepr.Str(action), pyrepr.Str(skillShown), restored, removed, safetyID), nil
}
