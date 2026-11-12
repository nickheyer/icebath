package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
)

// Per origin journal of prepared submissions, isolated like localStorage
type PreparedJournal struct {
	userData string
	file     string
	warn     func(string)
}

// Journal of an origin under the user data directory, warn receiving the quarantine notice
func NewPreparedJournal(userData, origin string, warn func(string)) *PreparedJournal {
	sum := sha256.Sum256([]byte(origin))
	return &PreparedJournal{userData: userData, file: filepath.Join(userData, "prepared-submissions-"+hex.EncodeToString(sum[:])+".json"), warn: warn}
}

// Journal entries, a torn journal moved aside with its bytes intact and read as empty
func (j *PreparedJournal) Read() (*Object, error) {
	raw, err := os.ReadFile(j.file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return NewObject(), nil
		}
		return nil, hardening.NodeErr(err)
	}
	text, _ := pycodecs.DecodeReplace(raw, "utf-8")
	if parsed, err := jsParse(text); err == nil {
		if obj, ok := parsed.(*Object); ok {
			return obj, nil
		}
	}
	aside := fmt.Sprintf("%s.corrupt-%d", j.file, time.Now().UnixMilli())
	if err := os.Rename(j.file, aside); err != nil {
		return nil, hardening.NodeErr(err)
	}
	j.warn("[prepared-submissions] quarantined unreadable journal " + filepath.Base(j.file))
	return NewObject(), nil
}

// Stores or with a nil entry deletes key and replaces the journal atomically and privately
func (j *PreparedJournal) write(journal *Object, key string, entry any) error {
	if entry == nil {
		journal.Delete(key)
	} else {
		journal.Set(key, entry)
	}
	if err := os.MkdirAll(j.userData, 0o777); err != nil {
		return hardening.NodeErr(err)
	}
	if err := hardening.WriteSecretFileAtomic(j.file, []byte(jsStringify(journal, "")), hardening.SecretFileOptions{}); err != nil {
		return hardening.NodeErr(err)
	}
	return nil
}

// Stores entry under key, a nil entry deleting it
func (j *PreparedJournal) Update(key string, entry any) error {
	journal, err := j.Read()
	if err != nil {
		return err
	}
	return j.write(journal, key, entry)
}

// Result of a compare and set, current being the record now stored under the key
type CompareAndSetResult struct {
	Applied bool
	Current any
}

// Replaces key only while it still holds exactly expected, nil meaning absent
func (j *PreparedJournal) CompareAndSet(key string, expected, entry any) (CompareAndSetResult, error) {
	journal, err := j.Read()
	if err != nil {
		return CompareAndSetResult{}, err
	}
	current, ok := journal.Get(key)
	if !ok {
		current = nil
	}
	if jsStringify(current, "") != jsStringify(expected, "") {
		return CompareAndSetResult{Applied: false, Current: current}, nil
	}
	if err := j.write(journal, key, entry); err != nil {
		return CompareAndSetResult{}, err
	}
	return CompareAndSetResult{Applied: true, Current: entry}, nil
}

// Origin of a URL as the URL standard serializes it, null for opaque origins
func URLOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "null"
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "blob" {
		return URLOrigin(u.Opaque + u.Path)
	}
	defaults := map[string]string{"http": "80", "https": "443", "ws": "80", "wss": "443", "ftp": "21"}
	def, special := defaults[scheme]
	if !special || u.Host == "" {
		return "null"
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && port != def {
		host += ":" + port
	}
	return scheme + "://" + host
}

// String or null argument as the IPC handlers type check them
func jsonStringOrNull(raw json.RawMessage) (*string, bool) {
	v, err := jsParse(string(raw))
	if err != nil {
		return nil, false
	}
	switch t := v.(type) {
	case nil:
		return nil, true
	case string:
		return &t, true
	}
	return nil, false
}

// Parses a JSON text argument of the renderer, nil standing for null
func parseJSONArg(s *string) (any, error) {
	if s == nil {
		return nil, nil
	}
	v, err := jsParse(*s)
	if err != nil {
		return nil, errors.New("SyntaxError: " + err.Error())
	}
	return v, nil
}

// Answers preparedSubmissions.read with the journal as JSON text
func (j *PreparedJournal) ReadText() (string, error) {
	journal, err := j.Read()
	if err != nil {
		return "", err
	}
	return jsStringify(journal, ""), nil
}

// Answers preparedSubmissions.update for a string key and a JSON text or null entry
func (j *PreparedJournal) UpdateText(key, entry json.RawMessage) error {
	k, kok := jsonStringOrNull(key)
	e, eok := jsonStringOrNull(entry)
	if !kok || k == nil || !eok {
		return errors.New("Invalid prepared submission")
	}
	value, err := parseJSONArg(e)
	if err != nil {
		return err
	}
	return j.Update(*k, value)
}

// Answers preparedSubmissions.compareAndSet, current coming back as JSON text or null
func (j *PreparedJournal) CompareAndSetText(key, expected, entry json.RawMessage) (*Object, error) {
	k, kok := jsonStringOrNull(key)
	x, xok := jsonStringOrNull(expected)
	e, eok := jsonStringOrNull(entry)
	if !kok || k == nil || !xok || !eok {
		return nil, errors.New("Invalid prepared submission comparison")
	}
	want, err := parseJSONArg(x)
	if err != nil {
		return nil, err
	}
	next, err := parseJSONArg(e)
	if err != nil {
		return nil, err
	}
	res, err := j.CompareAndSet(*k, want, next)
	if err != nil {
		return nil, err
	}
	out := NewObject()
	out.Set("applied", res.Applied)
	if res.Current == nil {
		out.Set("current", nil)
	} else {
		out.Set("current", jsStringify(res.Current, ""))
	}
	return out, nil
}
