package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/state"
)

var attachmentMimes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// Most attachments one submission may carry
const attachmentLimit = 10

// Paths one capture published or reused, leased until its admission commits or is refused
type captureScope struct {
	mu           sync.Mutex
	authority    *SessionAuthority
	held         []string
	allPaths     []string
	abandoned    bool
	writeStarted bool
}

type captureScopeKey struct{}

// Capture scope the context carries, nil outside a capture lease
func leaseScope(ctx context.Context) *captureScope {
	scope, _ := ctx.Value(captureScopeKey{}).(*captureScope)
	return scope
}

// Leases a path to the scope and records it among everything the scope published
func (s *captureScope) add(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = append(s.held, path)
	s.allPaths = append(s.allPaths, path)
}

// Marks the admission write of the scope begun, after which a committed row may own its bytes
func (s *captureScope) startWrite() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeStarted = true
}

// Whether the admission write of the scope began
func (s *captureScope) wrote() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeStarted
}

// Context holding every path captured under it against a concurrent release until the returned func runs
func captureLease(ctx context.Context, a *SessionAuthority) (context.Context, *captureScope, func()) {
	scope := &captureScope{authority: a}
	return context.WithValue(ctx, captureScopeKey{}, scope), scope, func() { dropCaptureLease(nil, nil, scope) }
}

// Context leasing every path captured under it with no admitting authority until the returned func runs
func CaptureLease(ctx context.Context) (context.Context, func()) {
	leased, _, release := captureLease(ctx, nil)
	return leased, release
}

// Ends a scope's lease early on the named paths, all of them when none is named
func dropCaptureLease(ctx context.Context, paths map[string]bool, scope *captureScope) {
	held := scope
	if held == nil && ctx != nil {
		held = leaseScope(ctx)
	}
	if held == nil {
		return
	}
	held.mu.Lock()
	defer held.mu.Unlock()
	if len(held.held) == 0 {
		return
	}
	mediaLeaseMu.Lock()
	defer mediaLeaseMu.Unlock()
	kept := held.held[:0:0]
	for _, path := range held.held {
		if paths != nil && !paths[path] {
			kept = append(kept, path)
			continue
		}
		mediaLeases[path]--
		if mediaLeases[path] <= 0 {
			delete(mediaLeases, path)
		}
	}
	held.held = kept
}

// Marks a capture no row can own abandoned and releases it once
func abandonCapture(ctx context.Context, scope *captureScope) error {
	if scope == nil || scope.authority == nil {
		return nil
	}
	scope.mu.Lock()
	if scope.abandoned {
		scope.mu.Unlock()
		return nil
	}
	scope.abandoned = true
	scope.mu.Unlock()
	return releaseAbandonedCapture(ctx, scope)
}

// Collects every path an abandoned capture published, also after its caller left
func releaseAbandonedCapture(ctx context.Context, scope *captureScope) error {
	if scope.authority == nil {
		return nil
	}
	dropCaptureLease(nil, nil, scope)
	scope.mu.Lock()
	paths := slices.Clone(scope.allPaths)
	scope.mu.Unlock()
	slices.Sort(paths)
	var references []any
	for _, path := range slices.Compact(paths) {
		references = append(references, pyjson.ObjectOf("path", path, "sha256", pypath.PathName(pypath.PathParent(pypath.PathStr(path)))))
	}
	if len(references) == 0 {
		return nil
	}
	_, err := releaseUnheldMedia(context.WithoutCancel(ctx), scope.authority.DB, references, true)
	return err
}

// Admission image type the bytes really are, empty for anything else
func sniffImageMime(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// Wire attachment as a plain mapping, false for anything that is no JSON object
func attachmentItem(item any) (map[string]any, bool) {
	switch v := item.(type) {
	case map[string]any:
		return v, true
	case *pyjson.Object:
		plain, ok := pyjson.Plain(v).(map[string]any)
		return plain, ok
	}
	return nil, false
}

// Committed payload fields of a submission's attachments, empty when it carries none
func admitAttachments(ctx context.Context, attachments any, admitted func() (*pyjson.Object, error)) (*pyjson.Object, error) {
	out := pyjson.NewObject()
	if attachments == nil {
		return out, nil
	}
	list, ok := attachments.([]any)
	if !ok || len(list) == 0 || len(list) > attachmentLimit {
		return nil, runtimeRefusal("invalid_params")
	}
	paths := make([]string, 0, len(list))
	mimes := make([]any, 0, len(list))
	for _, raw := range list {
		item, ok := attachmentItem(raw)
		path, isPath := item["path"].(string)
		mime, isMime := item["mime"].(string)
		if !ok || len(item) != 2 || !isPath || !isMime || !attachmentMimes[mime] {
			return nil, runtimeRefusal("invalid_params")
		}
		paths = append(paths, pypath.PathStr(path))
		mimes = append(mimes, mime)
	}
	cache, err := mediacache.ImageCacheDir(ctx, home.Dir(ctx))
	if err != nil {
		return nil, err
	}
	staging, err := pypath.Resolve(cache)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if !pypath.IsAbs(path) {
			return nil, runtimeRefusal("invalid_params")
		}
		resolved, err := pypath.Resolve(path)
		if err != nil {
			return nil, err
		}
		if !samePath(pypath.PathParent(resolved), staging) {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	var row, committed *pyjson.Object
	if admitted != nil {
		if row, err = admitted(); err != nil {
			return nil, err
		}
	}
	if row != nil {
		value, _ := rowPayload(row).Get("attachments_v1")
		committed, _ = value.(*pyjson.Object)
	}
	hashes, err := attachmentHashes(paths)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return nil, err
		}
		return nil, runtimeRefusal("invalid_params")
	}
	var media []any
	var committedTypes any
	if committed != nil {
		value, _ := committed.Get("media")
		media, _ = value.([]any)
		committedTypes, _ = committed.Get("media_types")
	}
	retry := false
	if slices.Contains(hashes, "") {
		if committed == nil || len(media) != len(paths) || !pyjson.Equal(committedTypes, mimes) {
			return nil, runtimeRefusal("invalid_params")
		}
		for i, reference := range media {
			if pypath.PathName(pypath.PathStr(rowString(reference.(*pyjson.Object), "path"))) != pypath.PathName(paths[i]) {
				return nil, runtimeRefusal("invalid_params")
			}
		}
		for i, digest := range hashes {
			if digest != "" && digest != rowString(media[i].(*pyjson.Object), "sha256") {
				return nil, state.NewRuntimeStoreError("admission_conflict", &state.AdmittedRequestConflictField, &state.AdmittedRequestConflictDetail)
			}
		}
		retry = true
	} else if committed != nil && pyjson.Equal(committedTypes, mimes) && len(media) == len(hashes) {
		retry = true
		for i, reference := range media {
			if rowString(reference.(*pyjson.Object), "sha256") != hashes[i] {
				retry = false
			}
		}
	}
	if retry {
		if rowString(row, "status") != AdmissionTerminal {
			if _, err := restoreNativeMedia(ctx, media); err != nil {
				return nil, err
			}
		}
		out.Set("attachments_v1", committed)
		return out, nil
	}
	captured, err := CaptureNativeMedia(ctx, paths)
	if err != nil {
		return nil, err
	}
	out.Set("attachments_v1", pyjson.ObjectOf("media", captured, "media_types", mimes))
	return out, nil
}

// SHA-256 of a delivery as received, its prepared payload and the provider payload the hook also sees
func ingressFingerprint(payload *pyjson.Object, raw any) (string, error) {
	native, _ := payload.Get("native_text_v1")
	source, ok := native.(*pyjson.Object)
	if !ok {
		return "", &pyKeyError{"native_text_v1"}
	}
	envelope := source.Copy()
	envelope.Delete("ingress_sha256")
	if value, ok := envelope.Get("media"); ok {
		list, _ := value.([]any)
		sized := make([]any, 0, len(list))
		for _, item := range list {
			media, _ := item.(*pyjson.Object)
			if media == nil {
				return "", strSubscriptError(item)
			}
			digest, _ := media.Get("sha256")
			size, _ := media.Get("size")
			sized = append(sized, []any{digest, size})
		}
		envelope.Set("media", sized)
	}
	received := payload.Copy()
	received.Set("native_text_v1", envelope)
	var value any = received
	if raw != nil {
		value = pyjson.ObjectOf("payload", received, "raw_message", raw)
	}
	canonical, err := pyjson.Dumps(value, pyjson.SortKeys(true), pyjson.Separators(",", ":"),
		pyjson.Default(func(v any) (any, error) { return pyrepr.Repr(v), nil }))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}

// Media references of a native payload, empty when it carries none
func nativeMedia(payload *pyjson.Object) []any {
	native, _ := payload.Get("native_text_v1")
	envelope, _ := native.(*pyjson.Object)
	if envelope == nil {
		return nil
	}
	value, _ := envelope.Get("media")
	list, _ := value.([]any)
	return list
}

// Digest and size pairs of media references in order
func mediaIdentities(references []any) []any {
	out := make([]any, 0, len(references))
	for _, item := range references {
		reference, _ := item.(*pyjson.Object)
		digest, _ := rowGet(reference, "sha256")
		size, _ := rowGet(reference, "size")
		out = append(out, []any{digest, size})
	}
	return out
}

// Payload a redelivered native message commits, the committed references standing in for an identical fresh capture
func reconcileNativeRetry(ctx context.Context, db *state.SessionDB, principalID, sessionID, requestID string, payload *pyjson.Object, raw any) (*pyjson.Object, error) {
	fresh := nativeMedia(payload)
	found, status, encoded := false, "", ""
	err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT status, payload_json FROM session_admissions WHERE principal_id=? AND "+
			"target_session_id=? AND request_id=?", principalID, sessionID, requestID)
		if err != nil {
			return err
		}
		defer rows.Close()
		if found = rows.Next(); found {
			if err := rows.Scan(&status, &encoded); err != nil {
				return err
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	retained := pyjson.NewObject()
	if found {
		decoded, err := pyjson.Loads(encoded)
		if err != nil {
			return nil, err
		}
		if obj, ok := decoded.(*pyjson.Object); ok {
			retained = obj
		}
	}
	var recorded any
	if native, ok := retained.Get("native_text_v1"); ok {
		recorded, _ = rowGet(native.(*pyjson.Object), "ingress_sha256")
	}
	var resolved *pyjson.Object
	if recorded != nil {
		fingerprint, err := ingressFingerprint(payload, raw)
		if err != nil {
			return nil, err
		}
		if recorded == fingerprint {
			resolved = retained
		}
	}
	if resolved == nil {
		committed := nativeMedia(retained)
		if len(fresh) == 0 || len(committed) == 0 || !pyjson.Equal(mediaIdentities(committed), mediaIdentities(fresh)) {
			return payload, nil
		}
		native, _ := payload.Get("native_text_v1")
		envelope := native.(*pyjson.Object).Copy()
		envelope.Set("media", committed)
		resolved = payload.Copy()
		resolved.Set("native_text_v1", envelope)
	}
	committed := nativeMedia(resolved)
	if len(committed) > 0 && status != AdmissionTerminal {
		if _, err := restoreNativeMedia(ctx, committed); err != nil {
			return nil, err
		}
	}
	kept := map[string]bool{}
	for _, item := range committed {
		kept[rowString(item.(*pyjson.Object), "path")] = true
	}
	var unowned []any
	dropped := map[string]bool{}
	for _, item := range fresh {
		if path := rowString(item.(*pyjson.Object), "path"); !kept[path] {
			unowned = append(unowned, item)
			dropped[path] = true
		}
	}
	if len(unowned) > 0 {
		dropCaptureLease(ctx, dropped, nil)
		if _, err := releaseUnheldMedia(ctx, db, unowned, false); err != nil {
			return nil, err
		}
	}
	return resolved, nil
}

// Digest of each staged attachment, empty for one already pruned, refused for a second name of a file
func attachmentHashes(paths []string) ([]string, error) {
	hashes := make([]string, 0, len(paths))
	for _, path := range paths {
		links, err := lstatNlink(path)
		if err == nil && links != 1 {
			return nil, runtimeRefusal("invalid_params")
		}
		digest := ""
		if err == nil {
			digest, err = fileSHA256(path)
		}
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			digest = ""
		}
		hashes = append(hashes, digest)
	}
	return hashes, nil
}

// SHA-256 of a local regular file
func fileSHA256(path string) (string, error) {
	source, err := openRegular(path)
	if err != nil {
		return "", err
	}
	defer source.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, source); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// Event media fields of a row's committed attachments, nil for a text only row
func restoreAttachments(ctx context.Context, payload *pyjson.Object) (urls []string, types []string, err error) {
	value, _ := payload.Get("attachments_v1")
	data, _ := value.(*pyjson.Object)
	if data == nil || data.Len() == 0 {
		return nil, nil, nil
	}
	media, ok := data.Get("media")
	if !ok {
		return nil, nil, &pyKeyError{"media"}
	}
	list, _ := media.([]any)
	if urls, err = restoreNativeMedia(ctx, list); err != nil {
		return nil, nil, err
	}
	mediaTypes, ok := data.Get("media_types")
	if !ok {
		return nil, nil, &pyKeyError{"media_types"}
	}
	typeList, _ := mediaTypes.([]any)
	types = make([]string, 0, len(typeList))
	for _, item := range typeList {
		types = append(types, pyrepr.Str(item))
	}
	return urls, types, nil
}

// Error of native media that is no local regular file
type mediaValueError struct{ message string }

func (e *mediaValueError) Error() string { return e.message }

// Opens a local regular file without following a final link
func openRegular(path string) (*os.File, error) {
	if !pypath.IsAbs(path) {
		return nil, &mediaValueError{"native media must be a local regular file"}
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, &mediaValueError{"native media must be a local regular file"}
	}
	file, err := os.OpenFile(path, os.O_RDONLY|oNonblock|oNoFollow, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, &mediaValueError{"native media must be a regular file"}
	}
	return file, nil
}

// Byte limit one admission's media may reach in total
func inboundMediaLimit(ctx context.Context) int64 {
	return max(0, mediacache.InboundMediaMaxBytes(ctx, home.Dir(ctx)))
}

// Captures staged files into the content addressed store as immutable bytes, returning their references
func CaptureNativeMedia(ctx context.Context, paths []string) ([]any, error) {
	references := []any{}
	limit := inboundMediaLimit(ctx)
	var total int64
	var staged []string
	defer func() {
		for _, temporary := range staged {
			_ = os.Remove(temporary)
		}
	}()
	for _, value := range paths {
		if err := captureFile(ctx, pypath.PathStr(value), limit, total, &references, &staged); err != nil {
			return nil, err
		}
		total = 0
		for _, reference := range references {
			size, _ := reference.(*pyjson.Object).Get("size")
			total += size.(int64)
		}
	}
	store, err := nativeInputsStore(ctx)
	if err != nil {
		return nil, err
	}
	for i, temporary := range staged {
		if i >= len(references) {
			break
		}
		reference := references[i].(*pyjson.Object)
		target := rowString(reference, "path")
		parent := pypath.PathParent(target)
		root := pypath.PathParent(parent)
		resolved, err := pypath.Resolve(parent)
		if err != nil {
			return nil, err
		}
		if !samePath(resolved, parent) {
			return nil, runtimeRefusal("invalid_params")
		}
		if _, err := home.MakeUserDir(ctx, filepath.Join(store, pypath.PathName(parent)), 0o700, false, true); err != nil {
			return nil, err
		}
		if err := publishCapture(ctx, temporary, target, reference); err != nil {
			return nil, err
		}
		up := pypath.PathParent(root)
		for _, directory := range []string{parent, root, up, pypath.PathParent(up), pypath.PathParent(pypath.PathParent(up))} {
			pyfs.FsyncDirectory(directory)
		}
	}
	return references, nil
}

// Publishes one private snapshot under its content address and leases it, reusing identical bytes already there
func publishCapture(ctx context.Context, temporary, target string, reference *pyjson.Object) error {
	scope := leaseScope(ctx)
	mediaLeaseMu.Lock()
	defer mediaLeaseMu.Unlock()
	if scope != nil {
		scope.add(target)
		mediaLeases[target]++
	}
	if _, err := os.Lstat(target); err == nil {
		if _, err := restoreNativeMedia(ctx, []any{reference}); err != nil {
			return err
		}
		now := time.Now()
		return os.Chtimes(target, now, now)
	}
	return os.Rename(temporary, target)
}

// Copies one staged file into a private snapshot under the store, bounded by the admission's byte limit
func captureFile(ctx context.Context, path string, limit, total int64, references *[]any, staged *[]string) error {
	source, err := openRegular(path)
	if err != nil {
		return runtimeRefusal("invalid_params")
	}
	defer source.Close()
	root, err := nativeInputsRoot(ctx)
	if err != nil {
		return err
	}
	resolved, err := pypath.Resolve(root)
	if err != nil {
		return err
	}
	if !samePath(resolved, root) {
		return runtimeRefusal("invalid_params")
	}
	store, err := nativeInputsStore(ctx)
	if err != nil {
		return err
	}
	if _, err := home.MakeUserDir(ctx, store, 0o700, true, true); err != nil {
		return err
	}
	output, err := os.CreateTemp(root, ".capture-")
	if err != nil {
		return err
	}
	*staged = append(*staged, output.Name())
	digest, size, err := copyBounded(ctx, source, output, limit, total)
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	hexDigest := hex.EncodeToString(digest.Sum(nil))
	target := filepath.Join(root, hexDigest, pypath.PathName(path))
	parent := pypath.PathParent(target)
	resolvedParent, err := pypath.Resolve(parent)
	if err != nil {
		return err
	}
	if !samePath(resolvedParent, parent) {
		return runtimeRefusal("invalid_params")
	}
	reference := pyjson.ObjectOf("path", target, "sha256", hexDigest, "size", size)
	if _, err := os.Lstat(target); err == nil {
		if _, err := restoreNativeMedia(ctx, []any{reference}); err != nil {
			return err
		}
	}
	*references = append(*references, reference)
	return nil
}

// Copies source to output in megabyte chunks, refusing once the admission would pass its byte limit
func copyBounded(ctx context.Context, source io.Reader, output *os.File, limit, total int64) (hash.Hash, int64, error) {
	digest := sha256.New()
	var size int64
	chunk := make([]byte, 1024*1024)
	for {
		n, err := source.Read(chunk)
		if n > 0 {
			size += int64(n)
			if mediacache.ValidateInboundMediaSize(ctx, home.Dir(ctx), total+size, "", &limit) != nil {
				return nil, 0, runtimeRefusal("invalid_params")
			}
			digest.Write(chunk[:n])
			if _, werr := output.Write(chunk[:n]); werr != nil {
				return nil, 0, werr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
	}
	return digest, size, output.Sync()
}

// Refuses a batch whose validated manifest sizes pass the admission's byte limit
func validateMediaBatchSize(ctx context.Context, sizes []int64) error {
	var sum int64
	for _, size := range sizes {
		sum += size
	}
	limit := inboundMediaLimit(ctx)
	if mediacache.ValidateInboundMediaSize(ctx, home.Dir(ctx), sum, "", &limit) != nil {
		return runtimeRefusal("invalid_params")
	}
	return nil
}

// Verified local paths of committed media references, refused when any is foreign or its bytes changed
func restoreNativeMedia(ctx context.Context, references []any) ([]string, error) {
	paths := []string{}
	if len(references) == 0 {
		return paths, nil
	}
	root, err := nativeInputsRoot(ctx)
	if err != nil {
		return nil, err
	}
	unavailable := runtimeRefusal("storage_unavailable")
	for _, item := range references {
		reference, ok := item.(*pyjson.Object)
		if !ok || reference.Len() != 3 {
			return nil, unavailable
		}
		rawPath, hasPath := reference.Get("path")
		digest, hasDigest := reference.Get("sha256")
		rawSize, hasSize := reference.Get("size")
		value, isPath := rawPath.(string)
		if !hasPath || !hasDigest || !hasSize || !isPath {
			return nil, unavailable
		}
		path := pypath.PathStr(value)
		resolved, err := pypath.Resolve(path)
		if err != nil {
			return nil, unavailable
		}
		parent := pypath.PathParent(path)
		if !samePath(pypath.PathParent(parent), root) || !samePath(resolved, path) || pypath.PathName(parent) != digest {
			return nil, unavailable
		}
		source, err := openRegular(path)
		if err != nil {
			return nil, unavailable
		}
		info, err := source.Stat()
		sum := sha256.New()
		if err == nil {
			_, err = io.Copy(sum, source)
		}
		source.Close()
		size := rowInt(reference, "size")
		if err != nil || rawSize == nil || size == nil || info.Size() != *size || hex.EncodeToString(sum.Sum(nil)) != digest {
			return nil, unavailable
		}
		paths = append(paths, path)
	}
	return paths, nil
}
