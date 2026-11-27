package skills

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Blobs no entry points to are kept this long before they are deleted, for a write in flight
const blobGrace = time.Hour

// Share of the size limit an over limit log is trimmed down to
const lowWater = 0.8

// Changes that remove a whole skill, whose before state takes every file of the skill, those on disk and those in the newest backup
var wholeSkill = map[string]bool{"delete": true, "archive": true, "purge": true}

// Folders never captured, being a tool's own state
var transient = map[string]bool{
	".venv": true, "venv": true, "env": true, ".env": true, "node_modules": true, "__pycache__": true, ".pytest_cache": true,
	".mypy_cache": true, ".ruff_cache": true, ".git": true,
}

// Top folders of the skills folder that hold no skill
var nonPackage = map[string]bool{".curator_backups": true, ".hub": true, ".archive": true, ".locks": true}

var (
	backupID     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z(-\d{2})?$`)
	archiveStamp = regexp.MustCompile(`^(.+)-\d{14}$`)
)

// One file of a change log entry: where it was and the blob holding what it held
type Item struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// One change to the skills: who made it, what it did to which skill, and the files before and after
type LogEntry struct {
	ID       string         `json:"id"`
	TS       string         `json:"ts"`
	Actor    string         `json:"actor"`
	Action   string         `json:"action"`
	Skill    string         `json:"skill"`
	Evidence map[string]any `json:"evidence"`
	Before   []Item         `json:"before"`
	After    []Item         `json:"after"`
}

// What one change recorded: the files before, captured already or read from a folder, and the folder after
type change struct {
	before     []Item
	captured   bool
	beforeRoot string
	after      string
	actor      string
	evidence   map[string]any
}

// The change log of the profile's skills
func (s *Scope) LogPath() string {
	return filepath.Join(s.Dir(), ".curator_ledger.jsonl")
}

// Folder of the blobs the change log points into, by digest
func (s *Scope) BlobDir() string {
	return filepath.Join(s.Home(), ".curator_backups", "blobs")
}

func (s *Scope) logLock(ctx context.Context) (context.Context, func(), error) {
	return hold(ctx, filepath.Join(s.Dir(), ".locks", "ledger.lock"))
}

// Whether changes to skills are logged
func (s *Scope) logging() bool {
	return s.Config.GetBool(skillsconfig.Ledger)
}

// Keeps the data as a blob named by its digest
func (s *Scope) store(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	dest := filepath.Join(s.BlobDir(), digest)
	if fsx.Exists(dest) {
		return digest, nil
	}
	return digest, fsx.Write(dest, data, 0o644)
}

// Data of a blob, false when the digest is malformed or the blob is gone
func (s *Scope) Blob(digest string) ([]byte, bool) {
	if digest == "" || strings.Trim(digest, "0123456789abcdef") != "" {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(s.BlobDir(), digest))
	return data, err == nil
}

// Regular files under a folder in path order, symlinked and transient folders not entered
func logFiles(root string) []string {
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return nil
		case d.IsDir() && p != root && transient[d.Name()]:
			return filepath.SkipDir
		case !d.IsDir() && fsx.IsFile(p):
			out = append(out, p)
		}
		return nil
	})
	return out
}

// Every file under the folder, or the file, kept as blobs; whole adds the skill's files only the newest backup has
func (s *Scope) snapshot(root string, whole bool, skill string) ([]Item, error) {
	if root == "" {
		return []Item{}, nil
	}
	root = filepath.Clean(root)
	var paths []string
	if info, err := os.Stat(root); err == nil {
		if info.Mode().IsRegular() {
			paths = []string{root}
		} else if info.IsDir() {
			paths = logFiles(root)
		}
	}
	items := []Item{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		digest, err := s.store(data)
		if err != nil {
			return nil, err
		}
		items = append(items, Item{Path: p, SHA256: digest})
	}
	if whole {
		return s.fill(root, items, skill)
	}
	return items, nil
}

// Path of a skill folder within the skills folder, none outside it or in a folder that holds no skill
func (s *Scope) packagePath(root string) string {
	rel, err := filepath.Rel(filepath.Clean(s.Dir()), filepath.Clean(root))
	if err != nil || !filepath.IsLocal(rel) {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if top, _, _ := strings.Cut(rel, "/"); nonPackage[top] {
		return ""
	}
	return rel
}

func unstamped(name string) string {
	if m := archiveStamp.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

// Folders of the SKILL.md files of a capture
func skillFolders(items []Item) []string {
	var out []string
	for _, item := range items {
		if filepath.Base(item.Path) == "SKILL.md" {
			out = append(out, filepath.Dir(filepath.Clean(item.Path)))
		}
	}
	return out
}

// Paths a skill can have in a backup: of its folder, of the folders its captured SKILL.md was in, its name with and without a stamp
func (s *Scope) prefixes(root, skill string, items []Item) []string {
	var candidates []string
	if root != "" {
		candidates = append(candidates, s.packagePath(root))
	}
	for _, dir := range skillFolders(items) {
		candidates = append(candidates, s.packagePath(dir))
	}
	candidates = append(candidates, skill)
	if skill != "" {
		candidates = append(candidates, unstamped(skill))
	}
	var out []string
	for _, c := range candidates {
		if c = strings.Trim(c, "/"); c != "" && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// Files of the skill in the newest curator backup of the skills folder, in archive order
func (s *Scope) backup(prefixes []string) ([]string, map[string][]byte) {
	backups := filepath.Join(s.Dir(), ".curator_backups")
	entries, err := os.ReadDir(backups)
	if err != nil || len(prefixes) == 0 {
		return nil, nil
	}
	newest := ""
	for _, entry := range entries {
		if entry.IsDir() && backupID.MatchString(entry.Name()) && fsx.IsFile(filepath.Join(backups, entry.Name(), "skills.tar.gz")) && entry.Name() > newest {
			newest = entry.Name()
		}
	}
	if newest == "" {
		return nil, nil
	}
	names, files, err := readBackup(filepath.Join(backups, newest, "skills.tar.gz"), prefixes)
	if err != nil {
		return nil, nil
	}
	return names, files
}

// Regular members of a gzip tar named by a prefix, or under one, never one leaving the archive
func readBackup(archive string, prefixes []string) ([]string, map[string][]byte, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, err
	}
	reader := tar.NewReader(gz)
	var names []string
	files := map[string][]byte{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.TrimLeft(strings.ReplaceAll(header.Name, `\`, "/"), "./")
		if name == "" || traversal(name) || !slices.ContainsFunc(prefixes, func(p string) bool { return name == p || strings.HasPrefix(name, p+"/") }) {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, nil, err
		}
		if _, seen := files[name]; !seen {
			names = append(names, name)
		}
		files[name] = data
	}
	return names, files, nil
}

// The capture with the skill's files only the newest backup has added, those on disk winning
func (s *Scope) fill(root string, items []Item, skill string) ([]Item, error) {
	out := slices.Clone(items)
	names, data := s.backup(s.prefixes(root, skill, out))
	if len(names) == 0 {
		return out, nil
	}
	skills := filepath.Clean(s.Dir())
	packages := map[string]bool{}
	if root != "" {
		root = filepath.Clean(root)
		packages[filepath.Base(root)] = true
		packages[unstamped(filepath.Base(root))] = true
	}
	have := map[string]bool{}
	for _, item := range out {
		if rel, err := filepath.Rel(skills, filepath.Clean(item.Path)); err == nil && filepath.IsLocal(rel) {
			have[filepath.ToSlash(rel)] = true
		}
	}
	for _, name := range names {
		parts := strings.Split(name, "/")
		base := skills
		if root != "" {
			base = root
			if packages[parts[0]] {
				parts = parts[1:]
			}
		}
		if len(parts) == 0 {
			continue
		}
		dest := filepath.Join(base, filepath.FromSlash(path.Join(parts...)))
		rel, err := filepath.Rel(skills, dest)
		if err != nil || !filepath.IsLocal(rel) || !within(dest, filepath.Clean(s.Home())) || have[filepath.ToSlash(rel)] {
			continue
		}
		digest, err := s.store(data[name])
		if err != nil {
			continue
		}
		out = append(out, Item{Path: dest, SHA256: digest})
		have[filepath.ToSlash(rel)] = true
	}
	return out, nil
}

// The captures without the files whose content is the same on both sides
func changed(before, after []Item) ([]Item, []Item) {
	was, is := map[string]string{}, map[string]string{}
	for _, item := range before {
		was[item.Path] = item.SHA256
	}
	for _, item := range after {
		is[item.Path] = item.SHA256
	}
	keep := func(items []Item) []Item {
		out := []Item{}
		for _, item := range items {
			if sha, ok := was[item.Path]; !ok || is[item.Path] != sha {
				out = append(out, item)
			}
		}
		return out
	}
	return keep(before), keep(after)
}

// Captures a folder before it changes, nothing when changes are not logged; a failed capture is told and the change goes ahead
func (s *Scope) capture(ctx context.Context, root string, whole bool, skill string) []Item {
	if !s.logging() {
		return nil
	}
	items, err := s.snapshot(root, false, skill)
	if err == nil && whole {
		items, err = s.fill(root, items, skill)
	}
	if err != nil {
		slog.Warn("skills: the change log could not capture a skill before it changed; the change goes ahead", "skill", skill, "err", err)
		return nil
	}
	return items
}

// Logs a change with the folder after it, never failing the change itself
func (s *Scope) log(ctx context.Context, action, skill string, c change) string {
	if !s.logging() {
		return ""
	}
	id, err := s.record(ctx, action, skill, c)
	if err != nil {
		slog.Warn("skills: the change log could not record a change; the change stands", "skill", skill, "action", action, "err", err)
	}
	return id
}

func (s *Scope) record(ctx context.Context, action, skill string, c change) (string, error) {
	whole := wholeSkill[action]
	before := c.before
	var err error
	switch {
	case !c.captured:
		before, err = s.snapshot(c.beforeRoot, whole, skill)
	case whole:
		before, err = s.fill(c.beforeRoot, before, skill)
	}
	if err != nil {
		return "", err
	}
	after, err := s.snapshot(c.after, false, "")
	if err != nil {
		return "", err
	}
	return s.write(ctx, action, skill, before, after, c.actor, c.evidence)
}

// Appends an entry, then keeps the log within its size limit
func (s *Scope) write(ctx context.Context, action, skill string, before, after []Item, who string, evidence map[string]any) (string, error) {
	if before == nil {
		before = []Item{}
	}
	if after == nil {
		after = []Item{}
	}
	if action != "pre-rollback" {
		before, after = changed(before, after)
	}
	if who != ActorAgent && who != ActorCurator && who != ActorUser {
		who = actor(ctx)
	}
	if evidence == nil {
		evidence = map[string]any{}
	}
	entry := LogEntry{ID: strs.Random(6), TS: now(), Actor: who, Action: action, Skill: skill, Evidence: evidence, Before: before, After: after}
	line, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.Dir(), 0o755); err != nil {
		return "", err
	}
	locked, release, err := s.logLock(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	f, err := os.OpenFile(s.LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	_, err = f.Write(append(line, '\n'))
	if err := errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	if limit := int64(s.Config.GetInt(skillsconfig.LedgerMaxBytes)); limit > 0 {
		if err := s.bound(locked, limit); err != nil {
			slog.Warn("skills: the change log could not be kept within its size; it is left as it is", "err", err)
		}
	}
	return entry.ID, nil
}

// Keeps the log within the limit: unchanged files dropped from every entry, then the oldest entries, then the blobs nothing points to
func (s *Scope) bound(ctx context.Context, limit int64) error {
	info, err := os.Stat(s.LogPath())
	if err != nil || info.Size() <= limit {
		return err
	}
	_, _, size, err := s.Compact(ctx)
	if err != nil {
		return err
	}
	if size <= int(float64(limit)*lowWater) {
		return nil
	}
	dropped, err := s.trim(ctx, int64(float64(limit)*lowWater))
	if err != nil || dropped == 0 {
		return err
	}
	_, _, err = s.Collect(ctx)
	return err
}

// Lines of the log, none when there is none
func (s *Scope) logLines() ([]string, error) {
	data, err := os.ReadFile(s.LogPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	scanner := bufio.NewScanner(bytes.NewReader(bytes.TrimPrefix(data, []byte("\uFEFF"))))
	scanner.Buffer(nil, 64<<20)
	for scanner.Scan() {
		out = append(out, scanner.Text())
	}
	return out, scanner.Err()
}

// Replaces the log with the lines at once, returning its new size
func (s *Scope) rewrite(lines []string) (int, error) {
	data := ""
	if len(lines) > 0 {
		data = strings.Join(lines, "\n") + "\n"
	}
	return len(data), fsx.Replace(s.LogPath(), []byte(data), 0o644)
}

// Drops the oldest lines until the log fits, the newest always kept; how many went
func (s *Scope) trim(ctx context.Context, limit int64) (int, error) {
	_, release, err := s.logLock(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	lines, err := s.logLines()
	if err != nil || len(lines) == 0 {
		return 0, err
	}
	var size int64
	start := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		add := int64(len(lines[i]) + 1)
		if start < len(lines) && size+add > limit {
			break
		}
		start, size = i, size+add
	}
	if start == 0 {
		return 0, nil
	}
	_, err = s.rewrite(lines[start:])
	return start, err
}

// Rewrites the log with unchanged files dropped from every entry: the entries kept, and its size before and after
func (s *Scope) Compact(ctx context.Context) (int, int, int, error) {
	_, release, err := s.logLock(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	defer release()
	lines, err := s.logLines()
	if err != nil {
		return 0, 0, 0, err
	}
	before := 0
	for _, line := range lines {
		before += len(line) + 1
	}
	var out []string
	kept := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry LogEntry
		if json.Unmarshal([]byte(line), &entry) != nil {
			out = append(out, line)
			continue
		}
		if entry.Action != "pre-rollback" {
			entry.Before, entry.After = changed(entry.Before, entry.After)
			data, err := json.Marshal(entry)
			if err != nil {
				return 0, 0, 0, err
			}
			line = string(data)
		}
		out = append(out, line)
		kept++
	}
	if slices.Equal(out, lines) {
		return kept, before, before, nil
	}
	after, err := s.rewrite(out)
	return kept, before, after, err
}

// Deletes the blobs no entry points to that are past the grace time: how many, and the bytes freed
func (s *Scope) Collect(ctx context.Context) (int, int64, error) {
	_, release, err := s.logLock(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer release()
	blobs, err := os.ReadDir(s.BlobDir())
	if err != nil {
		return 0, 0, nil
	}
	lines, err := s.logLines()
	if err != nil {
		return 0, 0, err
	}
	used := map[string]bool{}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry LogEntry
		if json.Unmarshal([]byte(line), &entry) != nil {
			return 0, 0, nil
		}
		for _, item := range append(entry.Before, entry.After...) {
			used[item.SHA256] = true
		}
	}
	deleted, freed := 0, int64(0)
	cutoff := time.Now().Add(-blobGrace)
	for _, blob := range blobs {
		p := filepath.Join(s.BlobDir(), blob.Name())
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || used[blob.Name()] || info.ModTime().After(cutoff) || os.Remove(p) != nil {
			continue
		}
		deleted++
		freed += info.Size()
	}
	return deleted, freed, nil
}

// Entries newest first, of one skill when named, at most limit of them unless it is negative
func (s *Scope) Entries(skill string, limit int) ([]LogEntry, error) {
	lines, err := s.logLines()
	if err != nil {
		return nil, err
	}
	var out []LogEntry
	for i := len(lines) - 1; i >= 0; i-- {
		var entry LogEntry
		if json.Unmarshal([]byte(lines[i]), &entry) != nil || skill != "" && entry.Skill != skill {
			continue
		}
		if limit >= 0 && len(out) == limit {
			break
		}
		out = append(out, entry)
	}
	return out, nil
}

// Entry of the id, false when there is none
func (s *Scope) Entry(id string) (LogEntry, bool, error) {
	entries, err := s.Entries("", -1)
	if err != nil || id == "" {
		return LogEntry{}, false, err
	}
	for _, entry := range entries {
		if entry.ID == id {
			return entry, true, nil
		}
	}
	return LogEntry{}, false, nil
}

// Refusal when an item lies outside the profile's folder
func (s *Scope) contained(items ...[]Item) string {
	home := filepath.Clean(s.Home())
	for _, list := range items {
		for _, item := range list {
			if p := filepath.Clean(item.Path); !within(p, home) {
				return fmt.Sprintf("entry references a path outside %s: %s", home, p)
			}
		}
	}
	return ""
}

// Puts back the files of an entry's before state, first logging what is there now so the rollback can be undone too
func (s *Scope) Rollback(ctx context.Context, id string) (string, error) {
	entry, ok, err := s.Entry(id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no ledger entry with id '%s'", id)
	}
	if problem := s.contained(entry.Before, entry.After); problem != "" {
		return "", errors.New("refusing rollback: " + problem)
	}
	before := entry.Before
	if wholeSkill[entry.Action] {
		root := ""
		if folders := skillFolders(before); len(folders) > 0 {
			root = folders[0]
		}
		if before, err = s.fill(root, before, entry.Skill); err != nil {
			return "", err
		}
		if problem := s.contained(before, entry.After); problem != "" {
			return "", errors.New("refusing rollback: " + problem)
		}
	}
	for _, item := range before {
		if _, ok := s.Blob(item.SHA256); !ok {
			return "", fmt.Errorf("missing blob %s for %s; rollback aborted, nothing was changed", item.SHA256, item.Path)
		}
	}
	var touched []string
	for _, item := range append(slices.Clone(before), entry.After...) {
		if item.Path != "" && !slices.Contains(touched, item.Path) {
			touched = append(touched, item.Path)
		}
	}
	slices.Sort(touched)
	safety := []Item{}
	for _, p := range touched {
		if !fsx.IsFile(p) {
			continue
		}
		data, err := os.ReadFile(p)
		if err == nil {
			var digest string
			if digest, err = s.store(data); err == nil {
				safety = append(safety, Item{Path: p, SHA256: digest})
				continue
			}
		}
		return "", fmt.Errorf("pre-rollback safety capture failed (%s); rollback aborted and current skills were not changed", pathError(err))
	}
	safetyID, err := s.write(ctx, "pre-rollback", entry.Skill, safety, safety, "", map[string]any{"rollback_target": id})
	if err != nil {
		return "", fmt.Errorf("pre-rollback safety capture failed (%s); rollback aborted and current skills were not changed", err)
	}
	restored := map[string]bool{}
	for _, item := range before {
		p := filepath.Clean(item.Path)
		data, _ := s.Blob(item.SHA256)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return "", err
		}
		restored[item.Path] = true
	}
	removed := 0
	for _, item := range entry.After {
		if item.Path == "" || restored[item.Path] || !fsx.IsFile(item.Path) {
			continue
		}
		if err := os.Remove(item.Path); err != nil {
			continue
		}
		removed++
	}
	s.write(ctx, "rollback", entry.Skill, safety, before, "", map[string]any{"rollback_target": id, "restored": len(before), "removed": removed})
	return fmt.Sprintf("rolled back entry %s (%s on '%s'): %d file(s) restored, %d removed. Safety entry %s captured the pre-rollback state.",
		id, entry.Action, entry.Skill, len(before), removed, safetyID), nil
}
