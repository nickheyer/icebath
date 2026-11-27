package sessions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Writes to the store are refused for good: its file was replaced underneath the process, or found corrupt
var ErrHalted = errors.New("The session database refuses further writes.")

type halted struct{ error }

func (h halted) Is(target error) bool { return target == ErrHalted }
func (h halted) Unwrap() error        { return h.error }

// Error of a store that refuses every write from now on, which errors.Is tells as ErrHalted
func Halted(err error) error {
	return halted{err}
}

// Keeps messages a store refused in sessions/<id>.jsonl under the folder, one per line, so none is lost; the file written
func Divert(dir, id string, messages ...*sessionsv1.Message) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" || len(messages) == 0 {
		return "", nil
	}
	folder := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(folder, id+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for _, m := range messages {
		line, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
		if err != nil {
			return "", err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return "", err
		}
	}
	return path, nil
}
