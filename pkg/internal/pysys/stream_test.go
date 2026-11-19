package pysys

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

// Blocks external HTTP then runs the suite
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

type reportingStream struct {
	bytes.Buffer
	tty bool
}

func (s *reportingStream) IsATTY() bool { return s.tty }

type wrapping struct {
	io.Writer
}

func (w wrapping) Unwrap() io.Writer { return w.Writer }

type fdStream struct {
	bytes.Buffer
	fd uintptr
}

func (s *fdStream) Fd() uintptr { return s.fd }

func TestAttrResolvesOnStreamThenOnWhatItWraps(t *testing.T) {
	inner := &reportingStream{tty: true}
	if s, ok := Attr[interface{ IsATTY() bool }](wrapping{inner}); !ok || !s.IsATTY() {
		t.Fatalf("attr through wrapper = %v %v", s, ok)
	}
	if _, ok := Attr[interface{ IsATTY() bool }](wrapping{&bytes.Buffer{}}); ok {
		t.Fatal("buffer has no isatty")
	}
	if _, ok := Attr[interface{ IsATTY() bool }](nil); ok {
		t.Fatal("nil stream has no isatty")
	}
}

func TestIsattyPrefersOwnAnswerThenDescriptor(t *testing.T) {
	if !Isatty(wrapping{&reportingStream{tty: true}}) {
		t.Fatal("wrapped reporter answers true")
	}
	if Isatty(&reportingStream{tty: false}) {
		t.Fatal("reporter answers false")
	}
	if Isatty(&bytes.Buffer{}) {
		t.Fatal("buffer is no terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if Isatty(wrapping{f}) {
		t.Fatal("regular file is no terminal")
	}
}

func TestFilenoReachesWrappedDescriptor(t *testing.T) {
	if fd, ok := Fileno(wrapping{&fdStream{fd: 7}}); !ok || fd != 7 {
		t.Fatalf("fileno = %d %v", fd, ok)
	}
	if _, ok := Fileno(&bytes.Buffer{}); ok {
		t.Fatal("buffer has no descriptor")
	}
	if fd, ok := Fileno(Stdout); !ok || fd != os.Stdout.Fd() {
		t.Fatalf("stdout fileno = %d %v", fd, ok)
	}
}
