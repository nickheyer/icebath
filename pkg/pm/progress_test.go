package pm

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestProgressCIStreamsAndAnExplicitChoiceOverridesIt(t *testing.T) {
	isolateHome(t)
	for _, tc := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{"CI": "true"}, true},
		{map[string]string{"GITHUB_ACTIONS": "true"}, true},
		{map[string]string{}, false},
		{map[string]string{"CI": "0"}, false},
		{map[string]string{"CI": "1", "ICEBATH_VERBOSE": "0"}, false},
		{map[string]string{"ICEBATH_VERBOSE": "1"}, true},
	} {
		if got := VerboseOutput(tc.env); got != tc.want {
			t.Fatalf("VerboseOutput(%v) = %v", tc.env, got)
		}
	}
}

// Child half of the contained run test, printing noise then the real error and exiting with the asked code
func TestProgressContainedChild(t *testing.T) {
	code := os.Getenv("RPT_PROGRESS_CHILD")
	if code == "" {
		return
	}
	for i := range 200 {
		fmt.Println("noise", i)
	}
	fmt.Println("the real error")
	n, _ := strconv.Atoi(code)
	os.Exit(n)
}

func TestProgressContainedRunShowsOutputOnlyWhenTheChildFails(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(strconv.FormatBool(fails), func(t *testing.T) {
			isolateHome(t)
			t.Setenv("ICEBATH_VERBOSE", "0")
			code := "0"
			if fails {
				code = "1"
			}
			var stream bytes.Buffer
			cmd := exec.Command(os.Args[0], "-test.run=^TestProgressContainedChild$")
			cmd.Env = append(os.Environ(), "RPT_PROGRESS_CHILD="+code)
			_, err := RunContained(cmd, "Installing things", ContainOptions{Stream: &stream})
			var called *CalledProcessError
			if fails != errors.As(err, &called) || (!fails && err != nil) {
				t.Fatalf("fails %v error %v", fails, err)
			}
			text := stream.String()
			if !strings.Contains(text, "→ Installing things") {
				t.Fatalf("no start line in %q", text)
			}
			if strings.Contains(text, "the real error") != fails {
				t.Fatalf("real error shown %v for fails %v", !fails, fails)
			}
			if strings.Contains(text, "noise 0") {
				t.Fatal("the failure tail is unbounded")
			}
		})
	}
}

// Byte decoding matches Python utf-8 errors replace, whole and split across writes
func TestProgressDecodesLikePythonReplace(t *testing.T) {
	isolateHome(t)
	for raw, want := range map[string]string{
		"\xe2\x82A": "�A", "\xed\xa0\x80": "���", "\xff\xfe": "��",
		"\xf0\x9f\x98": "�", "ok\xc3": "ok�", "\xc0\xaf": "��",
		"\xf4\x90\x80\x80": "����", "\xe0\x80\xaf": "���",
	} {
		if got, rest := decodeUTF8Replace([]byte(raw), true); got != want || rest != nil {
			t.Fatalf("decode %q = %q rest %q, want %q", raw, got, rest, want)
		}
	}
	label := "Installing things"
	var stream bytes.Buffer
	tail := NewLiveTail(&label, &stream, nil, "")
	tail.Write([]byte("\xe2\x9c"))
	tail.Write([]byte("\x93 ok\r\ntail\xe2"))
	tail.Close(false)
	if strings.Join(tail.Tail, "|") != "✓ ok|tail�" {
		t.Fatalf("tail %q", tail.Tail)
	}
	if got := universalNewlines("a\r\nb\rc\n"); got != "a\nb\nc\n" {
		t.Fatalf("newlines %q", got)
	}
}

// Child half of the signal test, killing itself the way an out of memory kill ends a child
func TestProgressKilledChild(t *testing.T) {
	if os.Getenv("RPT_PROGRESS_KILLED") != "1" {
		return
	}
	self, _ := os.FindProcess(os.Getpid())
	self.Signal(os.Kill)
	select {}
}

// A failed child reports its status the way subprocess CalledProcessError renders it
func TestProgressCalledProcessErrorMatchesPython(t *testing.T) {
	isolateHome(t)
	if got := (&CalledProcessError{Argv: []string{"a", "b"}, Code: 2}).Error(); got != "Command '['a', 'b']' returned non-zero exit status 2." {
		t.Fatalf("exit message %q", got)
	}
	if got := (&CalledProcessError{Argv: []string{"a", "b"}, Code: -9}).Error(); got != "Command '['a', 'b']' died with <Signals.SIGKILL: 9>." && runtime.GOOS != "windows" {
		t.Fatalf("signal message %q", got)
	}
	if got := (&CalledProcessError{Argv: []string{"a"}, Code: -40}).Error(); got != "Command '['a']' died with unknown signal 40." {
		t.Fatalf("unknown signal message %q", got)
	}
	t.Setenv("ICEBATH_VERBOSE", "0")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProgressKilledChild$")
	cmd.Env = append(os.Environ(), "RPT_PROGRESS_KILLED=1")
	var stream bytes.Buffer
	_, err := RunContained(cmd, "Killed", ContainOptions{Stream: &stream})
	var called *CalledProcessError
	if !errors.As(err, &called) {
		t.Fatalf("error %v", err)
	}
	want := -9
	if runtime.GOOS == "windows" {
		want = 1
	}
	if called.Code != want {
		t.Fatalf("code %d, want %d", called.Code, want)
	}
}
