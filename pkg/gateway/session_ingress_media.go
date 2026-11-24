package gateway

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

var (
	ingressAPIImageName = regexp.MustCompile(`^api_[0-9a-f]{32}\.(png|jpg|gif|webp)$`)
	ingressSHA256Name   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Retained paths an in flight capture holds against release until its admission commits or is refused
var (
	mediaLeaseMu sync.Mutex
	mediaLeases  = map[string]int{}
)

// Prompt line a hosted payload builder appends for each retained non image document
const HostedDocumentLine = "\n[Shared attachment] file: "

var hostedDocumentRe = regexp.MustCompile(regexp.QuoteMeta(HostedDocumentLine) + `([^\n]+)\n`)

// Unresolved retained native input store of the bound profile under its document cache
func nativeInputsStore(ctx context.Context) (string, error) {
	docs, err := mediacache.DocumentCacheDir(ctx, home.Dir(ctx))
	if err != nil {
		return "", err
	}
	return filepath.Clean(docs + string(filepath.Separator) + "native-inputs"), nil
}

// Resolved content addressed store of retained native inputs the identity checks compare against
func nativeInputsRoot(ctx context.Context) (string, error) {
	store, err := nativeInputsStore(ctx)
	if err != nil {
		return "", err
	}
	return pathutil.Resolve(store)
}

// Retained document references a hosted admission prompt names, empty for every other row
func HostedDocumentReferences(requestID string, payload *jsonutil.Object) ([]*jsonutil.Object, error) {
	return hostedDocumentReferences(context.Background(), requestID, payload)
}

// Content addressed documents under this profile root a hosted prompt names, never an API image name
func hostedDocumentReferences(ctx context.Context, requestID string, payload *jsonutil.Object) ([]*jsonutil.Object, error) {
	references := []*jsonutil.Object{}
	if !strings.HasPrefix(requestID, "hosted:") {
		return references, nil
	}
	if payload == nil {
		return nil, errors.New("the admission has no media section")
	}
	raw, _ := payload.Get("text")
	text, ok := raw.(string)
	if !ok {
		return references, nil
	}
	root, err := nativeInputsRoot(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range hostedDocumentRe.FindAllStringSubmatch(text, -1) {
		value := m[1]
		path := filepath.Clean(value)
		parent := filepath.Dir(filepath.Clean(path))
		digest := pathutil.Name(parent)
		if filepath.IsAbs(path) && samePath(filepath.Dir(filepath.Clean(parent)), root) && ingressSHA256Name.MatchString(digest) &&
			!ingressAPIImageName.MatchString(pathutil.Name(path)) {
			ref := jsonutil.NewObject()
			ref.Set("path", value)
			ref.Set("sha256", digest)
			references = append(references, ref)
		}
	}
	return references, nil
}

// Native attachment and text media references eligible as deletion candidates after terminal settlement
func AdmissionMediaReferences(payload *jsonutil.Object) ([]*jsonutil.Object, error) {
	if payload == nil {
		return nil, errors.New("the admission has no media section")
	}
	var items []any
	for _, key := range []string{"attachments_v1", "native_text_v1"} {
		section, has := payload.Get(key)
		if !has {
			continue
		}
		obj, ok := section.(*jsonutil.Object)
		if !ok {
			return nil, fmt.Errorf("the admission media section is %s, expected a mapping", errchain.Kind(section))
		}
		media, has := obj.Get("media")
		if !has {
			continue
		}
		listed, err := jsonList(media)
		if err != nil {
			return nil, err
		}
		items = append(items, listed...)
	}
	out := make([]*jsonutil.Object, 0, len(items))
	for _, item := range items {
		ref, ok := item.(*jsonutil.Object)
		if !ok {
			return nil, strSubscriptError(item)
		}
		out = append(out, ref)
	}
	return out, nil
}

// Python list() of a decoded JSON value, keys of a dict and characters of a str
func jsonList(v any) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case *jsonutil.Object:
		out := make([]any, 0, x.Len())
		for _, k := range x.Keys() {
			out = append(out, k)
		}
		return out, nil
	case string:
		out := []any{}
		for _, r := range x {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, fmt.Errorf("the admission media list is %s, expected a list", errchain.Kind(v))
}

// TypeError Python raises subscripting a non dict value with a str key
func strSubscriptError(v any) error {
	switch v.(type) {
	case string:
		return errors.New("an admission media entry is text, expected a mapping")
	case []any:
		return errors.New("an admission media entry is a list, expected a mapping")
	}
	return fmt.Errorf("an admission media entry is %s, expected a mapping", errchain.Kind(v))
}

// Value under key of a reference dict, the Python TypeError or KeyError otherwise
func referenceItem(reference any, key string) (any, error) {
	obj, ok := reference.(*jsonutil.Object)
	if !ok {
		return nil, strSubscriptError(reference)
	}
	v, has := obj.Get(key)
	if !has {
		return nil, errors.New("missing " + key)
	}
	return v, nil
}

// Refusal of a media path that is no text
func pathArgError(v any) error {
	return fmt.Errorf("a media path is %s, expected text", errchain.Kind(v))
}

// One release candidate, its raw path and the digest directory naming it
type mediaCandidate struct {
	path, sha256 string
}

// References whose existing bytes sit in their own digest directory directly under the retained root
func collectableMedia(references []any, root string) ([]mediaCandidate, error) {
	var out []mediaCandidate
	for _, reference := range references {
		raw, err := referenceItem(reference, "path")
		if err != nil {
			return nil, err
		}
		value, ok := raw.(string)
		if !ok {
			return nil, pathArgError(raw)
		}
		path := filepath.Clean(value)
		parent := filepath.Dir(filepath.Clean(path))
		if !samePath(filepath.Dir(filepath.Clean(parent)), root) {
			continue
		}
		digest, err := referenceItem(reference, "sha256")
		if err != nil {
			return nil, err
		}
		if s, ok := digest.(string); !ok || pathutil.Name(parent) != s {
			continue
		}
		info, err := pathutil.StatIfExists(path)
		if err != nil {
			return nil, err
		}
		if info != nil {
			out = append(out, mediaCandidate{path: value, sha256: pathutil.Name(parent)})
		}
	}
	return out, nil
}

const heldMediaSQL = `SELECT status, request_id, json_extract(payload_json,
            '$.attachments_v1.media', '$.native_text_v1.media', '$.api_turn_v1.media'),
            CASE WHEN status!='terminal' AND request_id LIKE 'hosted:%'
                 THEN json_extract(payload_json, '$.text') END
            FROM session_admissions WHERE status!='terminal'
            OR json_type(payload_json, '$.api_turn_v1.media') IS NOT NULL`

// Paths every live admission references plus API images every admission keeps as history context
func heldMediaPaths(ctx context.Context, conn state.SQLiteConn) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, heldMediaSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	held := map[string]bool{}
	for rows.Next() {
		var status, requestID, encoded, text any
		if err := rows.Scan(&status, &requestID, &encoded, &text); err != nil {
			return nil, err
		}
		encodedText, _ := encoded.(string)
		if b, ok := encoded.([]byte); ok {
			encodedText = string(b)
		}
		decoded, err := jsonutil.Decode(encodedText)
		if err != nil {
			return nil, err
		}
		triple, ok := decoded.([]any)
		if !ok || len(triple) != 3 {
			return nil, errors.New("not enough values to unpack (expected 3)")
		}
		var references []any
		for i, part := range []any{triple[2], triple[0], triple[1]} {
			if i > 0 && status == "terminal" {
				break
			}
			if !anyutil.IsTrue(part) {
				continue
			}
			listed, err := jsonList(part)
			if err != nil {
				return nil, err
			}
			references = append(references, listed...)
		}
		if status != "terminal" {
			payload := jsonutil.NewObject()
			payload.Set("text", text)
			hosted, err := hostedDocumentReferences(ctx, anyutil.String(requestID), payload)
			if err != nil {
				return nil, err
			}
			for _, h := range hosted {
				references = append(references, h)
			}
		}
		for _, reference := range references {
			path, err := referenceItem(reference, "path")
			if err != nil {
				return nil, err
			}
			switch p := path.(type) {
			case string:
				held[p] = true
			case []any:
				return nil, errors.New("a held media path is a list, expected text")
			case *jsonutil.Object:
				return nil, errors.New("a held media path is a mapping, expected text")
			default:
				return nil, pathArgError(p)
			}
		}
	}
	return held, rows.Err()
}

// Device and inode of a regular file, an error for anything whose identity is uncertain
func fileIdentity(path string) ([2]uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return [2]uint64{}, err
	}
	dev, ino, err := fsutil.Identity(path)
	if err != nil {
		return [2]uint64{}, err
	}
	if !info.Mode().IsRegular() || ino == 0 {
		return [2]uint64{}, errors.New("uncertain native file identity")
	}
	return [2]uint64{dev, ino}, nil
}

// Physical identities of every held path, false when any held path is foreign, aliased or unreadable
func heldFileIdentities(paths map[string]bool, root string) (map[[2]uint64]bool, bool) {
	identities := map[[2]uint64]bool{}
	for value := range paths {
		path := filepath.Clean(value)
		parent := filepath.Dir(filepath.Clean(path))
		if !samePath(filepath.Dir(filepath.Clean(parent)), root) {
			return nil, false
		}
		resolved, err := pathutil.Resolve(parent)
		if err != nil || !samePath(resolved, parent) {
			return nil, false
		}
		id, err := fileIdentity(path)
		if err != nil {
			return nil, false
		}
		identities[id] = true
	}
	return identities, true
}

const historyMentionsSQL = "WITH digests(value) AS MATERIALIZED (SELECT value FROM json_each(?)) SELECT content FROM messages " +
	"WHERE EXISTS (SELECT 1 FROM digests WHERE instr(content, digests.value))"

// Candidate paths any transcript row past afterID names raw or JSON escaped, in one messages pass
func historyMentions(ctx context.Context, conn state.SQLiteConn, references []mediaCandidate, afterID int64) (map[string]bool, error) {
	needles := map[string]string{}
	digests := map[string]bool{}
	for _, reference := range references {
		name := pathutil.Name(filepath.Clean(reference.path))
		for _, sep := range []string{"/", `\`} {
			tail := reference.sha256 + sep + name
			escaped, err := jsonutil.Encode(tail)
			if err != nil {
				return nil, err
			}
			needles[tail] = reference.path
			needles[escaped[1:len(escaped)-1]] = reference.path
		}
		digests[reference.sha256] = true
	}
	prefixes := []any{}
	for _, d := range slices.Sorted(maps.Keys(digests)) {
		prefixes = append(prefixes, d)
	}
	bound, err := jsonutil.Encode(prefixes)
	if err != nil {
		return nil, err
	}
	query, args := historyMentionsSQL, []any{bound}
	if afterID != 0 {
		query += " AND id>?"
		args = append(args, afterID)
	}
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	mentioned := map[string]bool{}
	for rows.Next() {
		var content any
		if err := rows.Scan(&content); err != nil {
			return nil, err
		}
		var text string
		if b, ok := content.([]byte); ok {
			if text, err = charsetutil.DecodeReplace(b, "utf-8"); err != nil {
				return nil, err
			}
		} else {
			text = anyutil.String(content)
		}
		for needle, raw := range needles {
			if strings.Contains(text, needle) {
				mentioned[raw] = true
			}
		}
	}
	return mentioned, rows.Err()
}

// Whether a failed ROLLBACK only found no open transaction, what sqlite3 in_transaction guards against
func noActiveTransaction(err error) bool {
	return strings.Contains(err.Error(), "no transaction is active")
}

// Unlinks the retained bytes no capture, accepted admission or retained transcript row still holds
func releaseUnheldMedia(ctx context.Context, db *state.SessionDB, references []any, retainHistory bool) (int, error) {
	root, err := nativeInputsRoot(ctx)
	if err != nil {
		return 0, err
	}
	candidates, err := collectableMedia(references, root)
	if err != nil || len(candidates) == 0 {
		return 0, err
	}
	var lastID int64
	if retainHistory {
		var seen map[string]bool
		err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
				return err
			}
			scanErr := conn.QueryRowContext(ctx, "SELECT coalesce(max(id), 0) FROM messages").Scan(&lastID)
			if scanErr == nil {
				seen, scanErr = historyMentions(ctx, conn, candidates, 0)
			}
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil && !noActiveTransaction(err) {
				return err
			}
			return scanErr
		})
		if err != nil {
			return 0, err
		}
		candidates = slices.DeleteFunc(candidates, func(c mediaCandidate) bool { return seen[c.path] })
		if len(candidates) == 0 {
			return 0, nil
		}
	}
	released := 0
	err = db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		released = 0
		held, err := heldMediaPaths(ctx, conn)
		if err != nil {
			return err
		}
		identities, ok := heldFileIdentities(held, root)
		if !ok {
			return nil
		}
		if retainHistory {
			mentioned, err := historyMentions(ctx, conn, candidates, lastID)
			if err != nil {
				return err
			}
			for p := range mentioned {
				held[p] = true
			}
		}
		for _, c := range candidates {
			if held[c.path] {
				continue
			}
			path := filepath.Clean(c.path)
			if !unlinkUnleased(c.path, path, identities) {
				continue
			}
			released++
			_ = os.Remove(filepath.Dir(filepath.Clean(path)))
		}
		return nil
	})
	return released, err
}

// Unlinks one candidate under the lease lock unless leased, reached through a link or a held alias
func unlinkUnleased(raw, path string, identities map[[2]uint64]bool) bool {
	mediaLeaseMu.Lock()
	defer mediaLeaseMu.Unlock()
	if mediaLeases[raw] > 0 {
		return false
	}
	parent := filepath.Dir(filepath.Clean(path))
	resolved, err := pathutil.Resolve(parent)
	if err != nil || !samePath(resolved, parent) {
		return false
	}
	id, err := fileIdentity(path)
	if err != nil || identities[id] {
		return false
	}
	return os.Remove(path) == nil
}

// Releases unheld retained media, the transcript keeping every reference it still names
func ReleaseUnheldMediaRetainingHistory(db *state.SessionDB, references []any) error {
	_, err := releaseUnheldMedia(context.Background(), db, references, true)
	return err
}
