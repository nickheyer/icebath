package pyfs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every line the iteration yields until it stops or fails
func collectLines(t *testing.T, data []byte, sig bool, stopAt int) ([]string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "text")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var lines []string
	err := ReadTextLines(path, sig, func(line string) bool {
		lines = append(lines, line)
		return len(lines) != stopAt
	})
	return lines, err
}

func TestReadTextLinesUniversalNewlinesAndSignature(t *testing.T) {
	lines, err := collectLines(t, []byte("\xef\xbb\xbfone\r\ntwo\rthree\nlast"), true, 0)
	if err != nil || !slices.Equal(lines, []string{"one\n", "two\n", "three\n", "last"}) {
		t.Fatalf("lines %q err %v", lines, err)
	}
	lines, err = collectLines(t, []byte("\xef\xbb\xbfone\n"), false, 0)
	if err != nil || !slices.Equal(lines, []string{"\ufeffone\n"}) {
		t.Fatalf("plain utf-8 keeps the mark, lines %q err %v", lines, err)
	}
}

func TestReadTextLinesStopsEarly(t *testing.T) {
	lines, err := collectLines(t, []byte("a\nb\nc\n"), false, 2)
	if err != nil || !slices.Equal(lines, []string{"a\n", "b\n"}) {
		t.Fatalf("lines %q err %v", lines, err)
	}
}

func TestReadTextLinesDecodeErrorRaisedAtItsChunk(t *testing.T) {
	head := "__version__ = \"1.0\"\n" + strings.Repeat("#\n", textChunkSize)
	lines, err := collectLines(t, []byte(head+"\xff\n"), false, 1)
	if err != nil || !slices.Equal(lines, []string{"__version__ = \"1.0\"\n"}) {
		t.Fatalf("a bad later chunk is never decoded, lines %q err %v", lines, err)
	}
	lines, err = collectLines(t, []byte(head+"\xff\n"), false, 0)
	var undecodable *UnicodeDecodeError
	if !errors.As(err, &undecodable) || len(lines) != 1+(2*textChunkSize-len("__version__ = \"1.0\"\n"))/2 {
		t.Fatalf("%d lines err %v", len(lines), err)
	}
	if _, err := collectLines(t, []byte("x\xffy\n"), false, 0); !errors.As(err, &undecodable) || undecodable.Start != 1 {
		t.Fatalf("first chunk err %v", err)
	}
}

func TestReadTextLinesMissingFile(t *testing.T) {
	err := ReadTextLines(filepath.Join(t.TempDir(), "absent"), true, func(string) bool { return true })
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err %v", err)
	}
}
