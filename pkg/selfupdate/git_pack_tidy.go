package selfupdate

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tidy budget seconds and the promisor pack count the merges work down to, both patched by hermes tests
var (
	TidyBudgetSeconds = 60.0
	PackCountTarget   = 50
)

const (
	tidyMinPackAgeSeconds = 60 * 60.0
	tidyMergeBatchBytes   = int64(256 * 1024 * 1024)
	tidyStateFile         = "icebath-pack-tidy.json"
	tidyStateVersion      = int64(2)
	tidyLockFile          = "icebath-pack-tidy.lock"
	idxFanout             = 8
	idxNames              = idxFanout + 256*4
)

var (
	tidyPackSuffixes = []string{".pack", ".idx", ".rev", ".bitmap", ".mtimes", ".promisor", ".keep"}
	idxV2            = []byte("\xfftOc\x00\x00\x00\x02")
)

// Counts of one tidy pass
type TidyResult struct {
	Erased     int
	FreedBytes int64
	Merged     int
	PacksLeft  int
	OutOfTime  bool
}

// Version 2 SHA-1 pack index held whole and searched in place
type packIndex struct {
	data  []byte
	count int
}

// Index opener, the _Index constructor hermes tests make fail for one pack
var openPackIndexFn = openPackIndex

// Reads a version 2 SHA-1 pack index, refusing any other format
func openPackIndex(idx string) (*packIndex, error) {
	data, err := os.ReadFile(idx)
	if err != nil {
		return nil, err
	}
	if len(data) < idxNames || !bytes.Equal(data[:8], idxV2) {
		return nil, fmt.Errorf("%s: not a v2 SHA-1 pack index", filepath.Base(idx))
	}
	count := int(binary.BigEndian.Uint32(data[idxFanout+255*4:]))
	if len(data) < idxNames+count*20 {
		return nil, fmt.Errorf("%s: truncated pack index", filepath.Base(idx))
	}
	return &packIndex{data: data, count: count}, nil
}

// Object names in index order, yielded lazily so a big index meets the deadline checks
func (x *packIndex) oids() iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		for i := 0; i < x.count; i++ {
			if !yield(x.data[idxNames+i*20 : idxNames+i*20+20]) {
				return
			}
		}
	}
}

// Binary search of one object name inside its fanout bucket
func (x *packIndex) contains(oid []byte) bool {
	lo := 0
	if oid[0] != 0 {
		lo = int(binary.BigEndian.Uint32(x.data[idxFanout+(int(oid[0])-1)*4:]))
	}
	hi := int(binary.BigEndian.Uint32(x.data[idxFanout+int(oid[0])*4:]))
	for lo < hi {
		mid := (lo + hi) / 2
		name := x.data[idxNames+mid*20 : idxNames+mid*20+20]
		switch c := bytes.Compare(name, oid); {
		case c == 0:
			return true
		case c < 0:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return false
}

// Releases the index bytes
func (x *packIndex) close() { x.data = nil }

// Unix seconds now
func tidyNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Modification time of a file as unix seconds
func tidyMtime(st os.FileInfo) float64 { return float64(st.ModTime().UnixNano()) / 1e9 }

// Whether a path exists, following symlinks
func tidyExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Noninteractive git environment that never lazy fetches, as a process environment list
func tidyGitEnv() []string {
	env := tools.NoninteractiveGitEnv(nil)
	env["GIT_NO_LAZY_FETCH"] = "1"
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// Path with its last suffix replaced, pathlib with_suffix
func withSuffix(path, suffix string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + suffix
}

// Pack name without its suffix, pathlib stem
func packStem(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// Promisor packs of the pack dir that still have their index
func promisorPacks(packDir string) []string {
	packs, _ := filepath.Glob(filepath.Join(packDir, "pack-*.pack"))
	var out []string
	for _, p := range packs {
		if tidyExists(withSuffix(p, ".promisor")) && tidyExists(withSuffix(p, ".idx")) {
			out = append(out, p)
		}
	}
	return out
}

// Whether a keep file pins the pack, an old fetch-pack or index-pack keep of a killed fetch pins nothing
func pinned(pack string, cutoff float64) bool {
	keep := withSuffix(pack, ".keep")
	raw, err := os.ReadFile(keep)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	st, err := os.Stat(keep)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	text := pystr.DecodeUTF8Replace(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
	fetchKeep := strings.HasPrefix(text, "fetch-pack ") || strings.HasPrefix(text, "index-pack ")
	return !(fetchKeep && tidyMtime(st) < cutoff)
}

// Path unlink of the tidy, the Path.unlink hermes tests patch
var tidyUnlinkFn = os.Remove

// Unlinks one pack part, clearing the read only bit first on Windows
func tidyUnlink(part string) error {
	if runtime.GOOS == "windows" {
		if err := os.Chmod(part, 0o666); err != nil {
			return err
		}
	}
	return tidyUnlinkFn(part)
}

// Removes the multi pack index and its incremental layers before any pack goes, so none can name a lost pack
func dropMidx(packDir string) error {
	if err := tidyUnlinkFn(filepath.Join(packDir, "multi-pack-index")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	layers := filepath.Join(packDir, "multi-pack-index.d")
	if err := tidyUnlinkFn(filepath.Join(layers, "multi-pack-index-chain")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	entries, _ := filepath.Glob(filepath.Join(layers, "*"))
	for _, layer := range entries {
		_ = tidyUnlink(layer)
	}
	_ = os.Remove(layers)
	return nil
}

// Deletes one pack's files payload first and returns the bytes freed, failing on the first part that will not go
func removePack(pack string) (int64, error) {
	if err := dropMidx(filepath.Dir(pack)); err != nil {
		return 0, err
	}
	var freed int64
	for _, suffix := range tidyPackSuffixes {
		part := withSuffix(pack, suffix)
		st, err := os.Stat(part)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return freed, err
		}
		if err := tidyUnlink(part); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return freed, err
		}
		freed += st.Size()
	}
	return freed, nil
}

// Finishes deletions an earlier run could not complete, pack files whose payload is already gone
func sweepRemnants(packDir string) {
	cutoff := tidyNow() - tidyMinPackAgeSeconds
	parts, _ := filepath.Glob(filepath.Join(packDir, "pack-*.*"))
	for _, part := range parts {
		if !slices.Contains(tidyPackSuffixes[1:], filepath.Ext(part)) || tidyExists(withSuffix(part, ".pack")) {
			continue
		}
		st, err := os.Stat(part)
		if err != nil {
			slog.Debug(fmt.Sprintf("pack remnant %s still in use: %s", filepath.Base(part), err))
			continue
		}
		if tidyMtime(st) < cutoff {
			if _, err := removePack(part); err != nil {
				slog.Debug(fmt.Sprintf("pack remnant %s still in use: %s", filepath.Base(part), err))
			}
		}
	}
}

// Packs proven unique and the merge batch size remembered between updates
type tidyState struct {
	kept       map[string]bool
	mergeBytes int64
}

// Stems of the packs in the pack dir, sorted
func livePackStems(packDir string) []string {
	packs, _ := filepath.Glob(filepath.Join(packDir, "pack-*.pack"))
	stems := make([]string, 0, len(packs))
	for _, p := range packs {
		stems = append(stems, packStem(p))
	}
	sort.Strings(stems)
	return stems
}

// String members of a json list value, false for anything that is not a list of strings
func stringSet(v any) (map[string]bool, bool) {
	out := map[string]bool{}
	if v == nil {
		return out, true
	}
	items, ok := v.([]any)
	if !ok {
		return nil, false
	}
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out[s] = true
	}
	return out, true
}

// Kept packs, seen packs and merge batch size of the state file, false when it is missing, foreign or malformed
func readTidyState(gitDir string) (map[string]bool, map[string]bool, int64, bool) {
	raw, err := os.ReadFile(filepath.Join(gitDir, tidyStateFile))
	if err != nil {
		return nil, nil, 0, false
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, nil, 0, false
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, nil, 0, false
	}
	state, ok := v.(*pyjson.Object)
	if !ok {
		return nil, nil, 0, false
	}
	if version, _ := state.Get("v"); version != tidyStateVersion {
		return nil, nil, 0, false
	}
	keptV, _ := state.Get("kept")
	kept, ok := stringSet(keptV)
	if !ok {
		return nil, nil, 0, false
	}
	seenV, _ := state.Get("seen")
	seen, ok := stringSet(seenV)
	if !ok {
		return nil, nil, 0, false
	}
	mergeBytes := tidyMergeBatchBytes
	if mergeV, present := state.Get("merge_bytes"); present && mergeV != nil {
		f, err := pyint.Float(mergeV)
		if err != nil {
			return nil, nil, 0, false
		}
		mergeBytes = int64(f)
	}
	return kept, seen, mergeBytes, true
}

// Loads the remembered state, forgetting the unique verdicts once a new pack appeared
func loadState(packDir string) *tidyState {
	kept, seen, mergeBytes, ok := readTidyState(filepath.Dir(filepath.Dir(packDir)))
	if !ok {
		kept, seen, mergeBytes = map[string]bool{}, map[string]bool{}, tidyMergeBatchBytes
	}
	for _, stem := range livePackStems(packDir) {
		if !seen[stem] {
			kept = map[string]bool{}
			break
		}
	}
	return &tidyState{kept: kept, mergeBytes: mergeBytes}
}

// Records the unique verdicts of the live packs, the packs seen and the merge batch size
func saveState(packDir string, state *tidyState) {
	live := livePackStems(packDir)
	kept := make([]any, 0, len(live))
	seen := make([]any, 0, len(live))
	for _, stem := range live {
		seen = append(seen, stem)
		if state.kept[stem] {
			kept = append(kept, stem)
		}
	}
	data := pyjson.NewObject()
	data.Set("v", tidyStateVersion)
	data.Set("kept", kept)
	data.Set("seen", seen)
	data.Set("merge_bytes", state.mergeBytes)
	text, err := pyjson.Dumps(data)
	if err == nil {
		err = os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(packDir)), tidyStateFile), []byte(text), 0o644)
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("could not record pack tidy state in %s: %s", packDir, err))
	}
}

// Whether every oid is in one of the other indexes, decided false when the deadline passed first
func isRedundant(oids iter.Seq[[]byte], others []*packIndex, deadline time.Time) (redundant, decided bool) {
	hit, n := 0, 0
	for oid := range oids {
		if n%1024 == 0 && time.Now().After(deadline) {
			return false, false
		}
		n++
		if others[hit].contains(oid) {
			continue
		}
		found := false
		for j, other := range others {
			if j != hit && other.contains(oid) {
				hit = j
				found = true
				break
			}
		}
		if !found {
			return false, true
		}
	}
	return true, true
}

// Erases promisor packs whose every object another promisor pack also holds, smallest candidates first
func eraseRedundantPacks(packDir string, deadline time.Time, result *TidyResult, state *tidyState) {
	cutoff := tidyNow() - tidyMinPackAgeSeconds
	sizes := map[string]int64{}
	var order, candidates []string
	for _, pack := range promisorPacks(packDir) {
		st, err := os.Stat(pack)
		if err != nil {
			continue
		}
		sizes[pack] = st.Size()
		order = append(order, pack)
		if !state.kept[packStem(pack)] && tidyMtime(st) < cutoff && !pinned(pack, cutoff) {
			candidates = append(candidates, pack)
		}
	}
	if len(candidates) == 0 {
		return
	}
	indexes := map[string]*packIndex{}
	defer func() {
		for _, idx := range indexes {
			idx.close()
		}
	}()
	for _, pack := range order {
		idx, err := openPackIndexFn(withSuffix(pack, ".idx"))
		if err != nil {
			slog.Debug(fmt.Sprintf("pack index %s unreadable: %s", filepath.Base(pack), err))
			continue
		}
		indexes[packStem(pack)] = idx
	}
	everyIndexRead := len(indexes) == len(sizes)
	bySize := append([]string{}, order...)
	sort.SliceStable(bySize, func(i, j int) bool { return sizes[bySize[i]] > sizes[bySize[j]] })
	sort.SliceStable(candidates, func(i, j int) bool { return sizes[candidates[i]] < sizes[candidates[j]] })
	for _, pack := range candidates {
		stem := packStem(pack)
		idx, ok := indexes[stem]
		if !ok {
			continue
		}
		var others []*packIndex
		for _, p := range bySize {
			if other, ok := indexes[packStem(p)]; ok && packStem(p) != stem {
				others = append(others, other)
			}
		}
		verdict, decided := false, true
		if len(others) > 0 {
			verdict, decided = isRedundant(idx.oids(), others, deadline)
		}
		if !decided {
			result.OutOfTime = true
			return
		}
		if !verdict {
			if everyIndexRead {
				state.kept[stem] = true
			}
			continue
		}
		idx.close()
		delete(indexes, stem)
		freed, err := removePack(pack)
		if err != nil {
			slog.Warn(fmt.Sprintf("Could not erase redundant pack %s (a later update retries): %s", filepath.Base(pack), err))
			continue
		}
		result.FreedBytes += freed
		result.Erased++
	}
}

// Runs git pack-objects over the batch into staging under custody, nil when out of time or unable to start
func mergeGit(ctx context.Context, repoRoot, staging string, batch []string, timeout time.Duration) (*CompletedProcess, error) {
	var input strings.Builder
	for _, p := range batch {
		input.WriteString(filepath.Base(p) + "\n")
	}
	in := input.String()
	done, err := RunGit(ctx, []string{"git"}, []string{"-c", "pack.packSizeLimit=0", "pack-objects", "-q", "--stdin-packs", filepath.Join(staging, "pack")},
		ChildOptions{Dir: repoRoot, Env: tidyGitEnv(), Input: &in, Capture: true, Text: true, Timeout: timeout, Attr: tools.WindowsHideAttr()})
	if err != nil {
		var refused *updaterecord.CustodyRefused
		if errors.As(err, &refused) {
			return nil, err
		}
		return nil, nil
	}
	return done, nil
}

// Last n runes of a text
func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// Merges the smallest promisor packs a batch at a time until the count is under target
func mergeSmallestPacks(ctx context.Context, repoRoot, packDir string, deadline time.Time, result *TidyResult, state *tidyState) error {
	staging := filepath.Join(filepath.Dir(filepath.Dir(packDir)), "icebath-tidy-staging")
	cutoff := tidyNow() - tidyMinPackAgeSeconds
	for {
		type sizedPack struct {
			size int64
			path string
		}
		var packs []sizedPack
		for _, p := range promisorPacks(packDir) {
			if pinned(p, cutoff) {
				continue
			}
			st, err := os.Stat(p)
			if err != nil {
				return err
			}
			packs = append(packs, sizedPack{st.Size(), p})
		}
		sort.Slice(packs, func(i, j int) bool {
			if packs[i].size != packs[j].size {
				return packs[i].size < packs[j].size
			}
			return packs[i].path < packs[j].path
		})
		if len(packs) <= PackCountTarget {
			return nil
		}
		var batch []string
		var total int64
		for _, sp := range packs {
			if len(batch) >= 2 && total+sp.size > state.mergeBytes {
				break
			}
			batch = append(batch, sp.path)
			total += sp.size
		}
		left := time.Until(deadline)
		if left <= 0 {
			result.OutOfTime = true
			return nil
		}
		_ = os.RemoveAll(staging)
		if err := os.Mkdir(staging, 0o777); err != nil {
			return err
		}
		stop, err := mergeBatch(ctx, repoRoot, packDir, staging, batch, left, result, state)
		_ = os.RemoveAll(staging)
		if err != nil || stop {
			return err
		}
	}
}

// One merge of a batch into a published pack, stop true when this run must not merge again
func mergeBatch(ctx context.Context, repoRoot, packDir, staging string, batch []string, left time.Duration, result *TidyResult, state *tidyState) (bool, error) {
	done, err := mergeGit(ctx, repoRoot, staging, batch, left)
	if err != nil {
		return true, err
	}
	if done == nil {
		result.OutOfTime = true
		state.mergeBytes = max(state.mergeBytes/2, 1)
		return true, nil
	}
	outputs, _ := filepath.Glob(filepath.Join(staging, "pack-*.idx"))
	if done.ReturnCode != 0 || len(outputs) != 1 {
		slog.Warn(fmt.Sprintf("Merging %d packs failed (%d outputs): %s", len(batch), len(outputs), tailRunes(strings.TrimSpace(done.Stderr), 300)))
		return true, nil
	}
	newBase := strings.TrimSuffix(outputs[0], ".idx")
	newName := filepath.Base(newBase)
	var promisor []byte
	for _, p := range batch {
		raw, err := os.ReadFile(withSuffix(p, ".promisor"))
		if err != nil {
			return true, err
		}
		promisor = append(promisor, raw...)
	}
	if err := os.WriteFile(filepath.Join(packDir, newName+".promisor"), promisor, 0o644); err != nil {
		return true, err
	}
	if tidyExists(newBase + ".rev") {
		if err := os.Rename(newBase+".rev", filepath.Join(packDir, newName+".rev")); err != nil {
			return true, err
		}
	}
	if err := os.Rename(newBase+".idx", filepath.Join(packDir, newName+".idx")); err != nil {
		return true, err
	}
	if err := os.Rename(newBase+".pack", filepath.Join(packDir, newName+".pack")); err != nil {
		return true, err
	}
	for _, pack := range batch {
		if packStem(pack) == newName {
			continue
		}
		if _, err := removePack(pack); err != nil {
			slog.Warn(fmt.Sprintf("Could not retire merged pack %s: %s", filepath.Base(pack), err))
		}
	}
	result.Merged += len(batch)
	return false, nil
}

// Kernel lock on the tidy lock file, nil while another tidy holds it or the filesystem cannot lock
func takeTidyLock(gitDir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(gitDir, tidyLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	held, err := pm.LockFD(f, false, 0)
	if err != nil {
		slog.Debug(fmt.Sprintf("pack tidy lock unavailable in %s: %s", gitDir, err))
	}
	if err != nil || !held {
		_ = f.Close()
		return nil, nil
	}
	return f, nil
}

// Spends at most the budget erasing redundant and merging small promisor packs, never failing the update
func TidyPartialClonePacks(ctx context.Context, repoRoot string, budgetSeconds float64) TidyResult {
	var result TidyResult
	packDir := filepath.Join(repoRoot, ".git", "objects", "pack")
	err := func() error {
		if st, err := os.Stat(packDir); err != nil || !st.IsDir() {
			return nil
		}
		filter, err := tools.PartialCloneFilter(ctx, repoRoot, pysubprocess.Options{Attr: tools.WindowsHideAttr()})
		if err != nil {
			return err
		}
		if filter == "" {
			return nil
		}
		deadline := time.Now().Add(time.Duration(budgetSeconds * float64(time.Second)))
		lock, err := takeTidyLock(filepath.Dir(filepath.Dir(packDir)))
		if err != nil || lock == nil {
			return err
		}
		state := loadState(packDir)
		runErr := func() error {
			sweepRemnants(packDir)
			eraseRedundantPacks(packDir, deadline, &result, state)
			if result.OutOfTime {
				return nil
			}
			return mergeSmallestPacks(ctx, repoRoot, packDir, deadline, &result, state)
		}()
		saveState(packDir, state)
		_ = lock.Close()
		if runErr != nil {
			return runErr
		}
		packs, _ := filepath.Glob(filepath.Join(packDir, "pack-*.pack"))
		result.PacksLeft = len(packs)
		return nil
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("partial-clone pack tidy failed in %s: %s", repoRoot, err))
	}
	return result
}
