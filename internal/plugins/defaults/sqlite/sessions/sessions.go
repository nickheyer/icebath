package sessions

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	_ "modernc.org/sqlite"

	"github.com/nickheyer/icebath/internal/config"
	sqliteconfig "github.com/nickheyer/icebath/internal/plugins/defaults/sqlite/config"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const name = "sqlite"

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	directory TEXT NOT NULL,
	created INTEGER NOT NULL,
	updated INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
	id INTEGER PRIMARY KEY,
	session TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
	data BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_session ON messages (session);
`

const columns = `id, title, directory, created, updated`

type store struct {
	config *config.Config

	mu sync.Mutex
	db *sql.DB
}

func Register(registry *sessions.Registry, cfg *config.Config) {
	registry.Register(&store{config: cfg})
}

func (s *store) Stores() map[string]sessions.Store {
	return map[string]sessions.Store{name: s}
}

func (s *store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *store) open() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db, nil
	}
	path := s.config.GetString(sqliteconfig.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=1")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	s.db = db
	return db, nil
}

func (s *store) Create(directory string) (*sessionsv1.Session, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	id, now := rand.Text(), time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO sessions (`+columns+`) VALUES (?, '', ?, ?, ?)`, id, directory, now, now); err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *store) List() ([]*sessionsv1.Session, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT ` + columns + ` FROM sessions ORDER BY updated DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*sessionsv1.Session
	for rows.Next() {
		session, err := scan(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, session)
	}
	return list, rows.Err()
}

func (s *store) Get(id string) (*sessionsv1.Session, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	session, err := scan(db.QueryRow(`SELECT `+columns+` FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sessions.ErrNotFound
	}
	return session, err
}

func (s *store) Rename(id, title string) error {
	return s.change(`UPDATE sessions SET title = ? WHERE id = ?`, title, id)
}

func (s *store) Delete(id string) error {
	return s.change(`DELETE FROM sessions WHERE id = ?`, id)
}

func (s *store) Messages(id string) ([]*sessionsv1.Message, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT data FROM messages WHERE session = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []*sessionsv1.Message
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		message := new(sessionsv1.Message)
		if err := proto.Unmarshal(data, message); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *store) Append(id string, message *sessionsv1.Message) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	data, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE sessions SET updated = ? WHERE id = ?`, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sessions.ErrNotFound
	}
	if _, err := tx.Exec(`INSERT INTO messages (session, data) VALUES (?, ?)`, id, data); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) Truncate(id string, keep int) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM messages WHERE session = ?1 AND id NOT IN (SELECT id FROM messages WHERE session = ?1 ORDER BY id LIMIT ?2)`, id, keep)
	return err
}

func (s *store) change(query string, args ...any) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	result, err := db.Exec(query, args...)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sessions.ErrNotFound
	}
	return nil
}

func scan(row interface{ Scan(...any) error }) (*sessionsv1.Session, error) {
	session := new(sessionsv1.Session)
	var created, updated int64
	if err := row.Scan(&session.Id, &session.Title, &session.Directory, &created, &updated); err != nil {
		return nil, err
	}
	session.Created = timestamppb.New(time.UnixMilli(created))
	session.Updated = timestamppb.New(time.UnixMilli(updated))
	return session, nil
}
