// Session store appending one JSON line per message so a crash loses at most the last line
package jsonl

import (
	"bufio"
	"bytes"
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

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

type line struct {
	Type    string          `json:"type"`
	Header  *session.Header `json:"header,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`
}

// What the store last wrote for a session, so later saves append only what is new
type mark struct {
	digests [][32]byte
	header  []byte
}

func digests(encoded [][]byte) [][32]byte {
	out := make([][32]byte, len(encoded))
	for i, raw := range encoded {
		out[i] = digest(raw)
	}
	return out
}

// Reports whether every message written so far still opens the session unchanged
func (m *mark) prefixOf(encoded [][]byte) bool {
	if len(m.digests) > len(encoded) {
		return false
	}
	for i, d := range m.digests {
		if digest(encoded[i]) != d {
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

func digest(data []byte) [32]byte { return sha256.Sum256(data) }

// Appends the new messages and a header when it changed, rewriting the log when history was altered
func (st *Store) Save(s *session.Session) error {
	target, err := st.Path(s.ID)
	if err != nil {
		return err
	}
	msgs := s.Snapshot()
	encoded := make([][]byte, len(msgs))
	for i, m := range msgs {
		raw, err := session.MarshalMessage(m)
		if err != nil {
			return err
		}
		encoded[i] = raw
	}
	header, err := json.Marshal(line{Type: "header", Header: ptr(s.Header())})
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.marks == nil {
		st.marks = map[string]*mark{}
	}
	if err := os.MkdirAll(st.Dir, 0o755); err != nil {
		return err
	}
	m, known := st.marks[s.ID]
	_, statErr := os.Stat(target)
	if !known || statErr != nil || !m.prefixOf(encoded) {
		if err := rewrite(target, header, encoded); err != nil {
			return err
		}
		st.marks[s.ID] = &mark{digests: digests(encoded), header: header}
		return nil
	}
	var buf bytes.Buffer
	for _, raw := range encoded[len(m.digests):] {
		entry, err := json.Marshal(line{Type: "message", Message: raw})
		if err != nil {
			return err
		}
		buf.Write(entry)
		buf.WriteByte('\n')
	}
	if !bytes.Equal(header, m.header) {
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
	if err := f.Close(); err != nil {
		return err
	}
	m.digests, m.header = digests(encoded), header
	return nil
}

func ptr(h session.Header) *session.Header { return &h }

// Writes a complete log through a temporary file
func rewrite(target string, header []byte, encoded [][]byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".*.tmp")
	if err != nil {
		return err
	}
	w := bufio.NewWriter(tmp)
	w.Write(header)
	w.WriteByte('\n')
	for _, raw := range encoded {
		entry, err := json.Marshal(line{Type: "message", Message: raw})
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return err
		}
		w.Write(entry)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Replays the log, the latest header winning, and cuts off an unterminated final line
func (st *Store) Load(id string) (*session.Session, error) {
	target, err := st.Path(id)
	if err != nil {
		return nil, err
	}
	s, msgs, good, err := read(target)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(target); err == nil && info.Size() > good {
		if err := os.Truncate(target, good); err != nil {
			return nil, err
		}
	}
	loaded := session.FromHeader(s, msgs)
	header, err := json.Marshal(line{Type: "header", Header: ptr(loaded.Header())})
	if err != nil {
		return nil, err
	}
	encoded := make([][]byte, len(msgs))
	for i, msg := range msgs {
		if encoded[i], err = session.MarshalMessage(msg); err != nil {
			return nil, err
		}
	}
	st.mu.Lock()
	if st.marks == nil {
		st.marks = map[string]*mark{}
	}
	st.marks[id] = &mark{digests: digests(encoded), header: header}
	st.mu.Unlock()
	return loaded, nil
}

// Parses every terminated line and reports where the last one ended
func read(target string) (session.Header, []protocol.Message, int64, error) {
	f, err := os.Open(target)
	if errors.Is(err, fs.ErrNotExist) {
		return session.Header{}, nil, 0, session.ErrNotFound
	}
	if err != nil {
		return session.Header{}, nil, 0, err
	}
	defer f.Close()
	var header *session.Header
	var msgs []protocol.Message
	br := bufio.NewReader(f)
	n := 0
	var good int64
	for {
		raw, err := br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return session.Header{}, nil, 0, err
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
			return session.Header{}, nil, 0, fmt.Errorf("%s: line %d: %w", target, n, err)
		}
		switch l.Type {
		case "header":
			if l.Header == nil {
				return session.Header{}, nil, 0, fmt.Errorf("%s: line %d: header without a body", target, n)
			}
			header = l.Header
		case "message":
			m, err := session.UnmarshalMessage(l.Message)
			if err != nil {
				return session.Header{}, nil, 0, fmt.Errorf("%s: line %d: %w", target, n, err)
			}
			msgs = append(msgs, m)
		default:
			return session.Header{}, nil, 0, fmt.Errorf("%s: line %d: unknown type %q", target, n, l.Type)
		}
	}
	if header == nil {
		return session.Header{}, nil, 0, fmt.Errorf("%s: no header", target)
	}
	return *header, msgs, good, nil
}

// Lists stored sessions newest first by replaying every log
func (st *Store) List() ([]session.Info, error) {
	entries, err := os.ReadDir(st.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var infos []session.Info
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || e.IsDir() || !session.ValidID(id) {
			continue
		}
		h, msgs, _, err := read(filepath.Join(st.Dir, e.Name()))
		if err != nil {
			return nil, err
		}
		infos = append(infos, session.Info{ID: h.ID, Created: h.Created, Updated: h.Updated, Title: h.Title, Model: h.Model, Parent: h.Parent, Labels: h.Labels, Messages: len(msgs), Usage: h.Usage})
	}
	session.SortInfos(infos)
	return infos, nil
}

func (st *Store) Delete(id string) error {
	target, err := st.Path(id)
	if err != nil {
		return err
	}
	st.mu.Lock()
	delete(st.marks, id)
	st.mu.Unlock()
	err = os.Remove(target)
	if errors.Is(err, fs.ErrNotExist) {
		return session.ErrNotFound
	}
	return err
}
