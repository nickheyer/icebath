// Session store appending one JSON line per message so a crash loses at most the last line
package jsonl

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/flock"
	"github.com/nickheyer/icebath/pkg/session"
)

// Header lines appended between rewrites before the log is compacted into a fresh file
const MaxHeaders = 32

type line struct {
	Type    string          `json:"type"`
	Header  *session.Header `json:"header,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`
	Record  json.RawMessage `json:"record,omitempty"`
}

// What the store last wrote for a session, so later saves append only what is new
type mark struct {
	entries [][32]byte
	records [][32]byte
	header  []byte
	size    int64
	headers int
}

func digests(encoded [][]byte) [][32]byte {
	out := make([][32]byte, len(encoded))
	for i, raw := range encoded {
		out[i] = sha256.Sum256(raw)
	}
	return out
}

// Reports whether every line written so far still opens the sequence unchanged
func prefixOf(written [][32]byte, encoded [][]byte) bool {
	if len(written) > len(encoded) {
		return false
	}
	for i, d := range written {
		if sha256.Sum256(encoded[i]) != d {
			return false
		}
	}
	return true
}

// Directory of append only session logs named by id
type Store struct {
	Dir   string
	mu    sync.Mutex
	marks map[string]*mark
}

// Path of the log for an id
func (st *Store) Path(id string) (string, error) {
	if !session.ValidID(id) {
		return "", session.InvalidID(id)
	}
	return filepath.Join(st.Dir, id+".jsonl"), nil
}

func (st *Store) lockPath(id string) string { return filepath.Join(st.Dir, id+".lock") }

// Appends the new lines and a header when it changed, rewriting the log when history was altered
func (st *Store) Save(ctx context.Context, s *session.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := s.ID()
	target, err := st.Path(id)
	if err != nil {
		return err
	}
	entries, err := encodeEntries(s.Entries())
	if err != nil {
		return err
	}
	records, err := encodeRecords(s.Records())
	if err != nil {
		return err
	}
	header, err := json.Marshal(line{Type: "header", Header: ptr(s.Header())})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(st.Dir, 0o755); err != nil {
		return err
	}
	lock, err := flock.Acquire(st.lockPath(id))
	if err != nil {
		return err
	}
	defer lock.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.marks == nil {
		st.marks = map[string]*mark{}
	}
	m, known := st.marks[id]
	info, statErr := os.Stat(target)
	fresh := !known || statErr != nil || info.Size() != m.size || !prefixOf(m.entries, entries) || !prefixOf(m.records, records)
	if !fresh && !bytes.Equal(header, m.header) && m.headers >= MaxHeaders {
		fresh = true
	}
	if fresh {
		size, err := rewrite(target, header, s.Entries(), entries, s.Records(), records)
		if err != nil {
			return err
		}
		st.marks[id] = &mark{entries: digests(entries), records: digests(records), header: header, size: size}
		return nil
	}
	var buf bytes.Buffer
	for _, raw := range entries[len(m.entries):] {
		writeLine(&buf, line{Type: "message", Message: raw})
	}
	for _, raw := range records[len(m.records):] {
		writeLine(&buf, line{Type: "record", Record: raw})
	}
	changed := !bytes.Equal(header, m.header)
	if changed {
		buf.Write(header)
		buf.WriteByte('\n')
	}
	if buf.Len() == 0 {
		return nil
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	m.entries, m.records, m.header = digests(entries), digests(records), header
	m.size += int64(buf.Len())
	if changed {
		m.headers++
	}
	return nil
}

func writeLine(buf *bytes.Buffer, l line) {
	raw, _ := json.Marshal(l)
	buf.Write(raw)
	buf.WriteByte('\n')
}

func ptr(h session.Header) *session.Header { return &h }

func encodeEntries(entries []session.Entry) ([][]byte, error) {
	out := make([][]byte, len(entries))
	for i, e := range entries {
		raw, err := session.MarshalEntry(e)
		if err != nil {
			return nil, err
		}
		out[i] = raw
	}
	return out, nil
}

func encodeRecords(records []session.Record) ([][]byte, error) {
	out := make([][]byte, len(records))
	for i, r := range records {
		raw, err := session.MarshalRecord(r)
		if err != nil {
			return nil, err
		}
		out[i] = raw
	}
	return out, nil
}

// Writes a complete log through a synced temporary file, entries in session order with records placed by sequence
func rewrite(target string, header []byte, entries []session.Entry, encEntries [][]byte, records []session.Record, encRecords [][]byte) (int64, error) {
	var buf bytes.Buffer
	buf.Write(header)
	buf.WriteByte('\n')
	next := 0
	for i, e := range entries {
		for next < len(records) && records[next].Seq < e.Seq {
			writeLine(&buf, line{Type: "record", Record: encRecords[next]})
			next++
		}
		writeLine(&buf, line{Type: "message", Message: encEntries[i]})
	}
	for ; next < len(records); next++ {
		writeLine(&buf, line{Type: "record", Record: encRecords[next]})
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, filepath.Base(target)+".*.tmp")
	if err != nil {
		return 0, err
	}
	fail := func(err error) (int64, error) {
		tmp.Close()
		os.Remove(tmp.Name())
		return 0, err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return 0, err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		os.Remove(tmp.Name())
		return 0, err
	}
	if err := syncDir(dir); err != nil {
		return 0, err
	}
	return int64(buf.Len()), nil
}

// Flushes the directory entry so a rename survives power loss where the file system allows it
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

// Replays the log, the latest header winning, ignoring an unterminated final line without touching the file
func (st *Store) Load(ctx context.Context, id string) (*session.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := st.Path(id)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(target); errors.Is(err, fs.ErrNotExist) {
		return nil, session.ErrNotFound
	}
	lock, err := flock.Acquire(st.lockPath(id))
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	h, entries, records, good, err := read(target)
	if err != nil {
		return nil, err
	}
	loaded := session.FromHeader(h, entries, records)
	header, err := json.Marshal(line{Type: "header", Header: ptr(loaded.Header())})
	if err != nil {
		return nil, err
	}
	encEntries, err := encodeEntries(loaded.Entries())
	if err != nil {
		return nil, err
	}
	encRecords, err := encodeRecords(loaded.Records())
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	if st.marks == nil {
		st.marks = map[string]*mark{}
	}
	st.marks[id] = &mark{entries: digests(encEntries), records: digests(encRecords), header: header, size: good}
	st.mu.Unlock()
	return loaded, nil
}

// Parses every terminated line and reports where the last one ended
func read(target string) (session.Header, []session.Entry, []session.Record, int64, error) {
	f, err := os.Open(target)
	if errors.Is(err, fs.ErrNotExist) {
		return session.Header{}, nil, nil, 0, session.ErrNotFound
	}
	if err != nil {
		return session.Header{}, nil, nil, 0, err
	}
	defer f.Close()
	var header *session.Header
	var entries []session.Entry
	var records []session.Record
	br := bufio.NewReader(f)
	n := 0
	var good int64
	for {
		raw, err := br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return session.Header{}, nil, nil, 0, err
		}
		if err == io.EOF {
			break
		}
		n++
		good += int64(len(raw))
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var l line
		if err := json.Unmarshal(raw, &l); err != nil {
			return session.Header{}, nil, nil, 0, fmt.Errorf("%s: line %d: %w", target, n, err)
		}
		switch l.Type {
		case "header":
			if l.Header == nil {
				return session.Header{}, nil, nil, 0, fmt.Errorf("%s: line %d: header without a body", target, n)
			}
			header = l.Header
		case "message":
			e, err := session.UnmarshalEntry(l.Message)
			if err != nil {
				return session.Header{}, nil, nil, 0, fmt.Errorf("%s: line %d: %w", target, n, err)
			}
			entries = append(entries, e)
		case "record":
			r, err := session.UnmarshalRecord(l.Record)
			if err != nil {
				return session.Header{}, nil, nil, 0, fmt.Errorf("%s: line %d: %w", target, n, err)
			}
			records = append(records, r)
		default:
			return session.Header{}, nil, nil, 0, fmt.Errorf("%s: line %d: unknown type %q", target, n, l.Type)
		}
	}
	if header == nil {
		return session.Header{}, nil, nil, 0, fmt.Errorf("%s: no header", target)
	}
	return *header, entries, records, good, nil
}

// Lists stored sessions newest first from the last complete header line of each log
func (st *Store) List(ctx context.Context) ([]session.Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dirEntries, err := os.ReadDir(st.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var infos []session.Info
	for _, e := range dirEntries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || e.IsDir() || !session.ValidID(id) {
			continue
		}
		h, err := tailHeader(filepath.Join(st.Dir, e.Name()))
		if err != nil {
			infos = append(infos, session.Info{ID: id, Error: err.Error()})
			continue
		}
		infos = append(infos, h.Info())
	}
	session.SortInfos(infos)
	return infos, nil
}

const tailChunk = 64 << 10

var headerPrefix = []byte(`{"type":"header"`)

// Finds the last complete header line by scanning the file backwards in chunks
func tailHeader(path string) (session.Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return session.Header{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return session.Header{}, err
	}
	size := info.Size()
	var tail []byte
	end := size
	for end > 0 {
		start := max(0, end-tailChunk)
		chunk := make([]byte, end-start)
		if _, err := f.ReadAt(chunk, start); err != nil && err != io.EOF {
			return session.Header{}, err
		}
		tail = append(chunk, tail...)
		if h, ok := lastHeader(tail, start == 0); ok {
			return h, nil
		}
		end = start
	}
	return session.Header{}, fmt.Errorf("%s: no header", path)
}

// Parses the last header line that is complete within buf, which begins at a line start when atStart
func lastHeader(buf []byte, atStart bool) (session.Header, bool) {
	lines := bytes.Split(buf, []byte{'\n'})
	first := 0
	if !atStart {
		first = 1
	}
	for i := len(lines) - 2; i >= first; i-- {
		raw := bytes.TrimSpace(lines[i])
		if !bytes.HasPrefix(raw, headerPrefix) {
			continue
		}
		var l line
		if err := json.Unmarshal(raw, &l); err != nil || l.Header == nil {
			continue
		}
		return *l.Header, true
	}
	return session.Header{}, false
}

func (st *Store) Exists(ctx context.Context, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	target, err := st.Path(id)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (st *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target, err := st.Path(id)
	if err != nil {
		return err
	}
	st.mu.Lock()
	delete(st.marks, id)
	st.mu.Unlock()
	if _, err := os.Stat(target); errors.Is(err, fs.ErrNotExist) {
		return session.ErrNotFound
	}
	lock, err := flock.Acquire(st.lockPath(id))
	if err != nil {
		return err
	}
	err = os.Remove(target)
	lock.Unlock()
	os.Remove(st.lockPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return session.ErrNotFound
	}
	return err
}
