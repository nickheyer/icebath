//go:build windows

package tuiserver

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Names the stdin boundary fixture a re-executed test binary runs in place of the tests
const entryTestChildEnv = "ICEBATH_TEST_ENTRY_CHILD"

// Environment of this process with the fixture mode replaced by mode
func entryChildEnv(mode string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), entryTestChildEnv+"=") {
			env = append(env, kv)
		}
	}
	return append(env, entryTestChildEnv+"="+mode)
}

// Gateway side moves its RPC stdin aside, runs a child on the standard input, then reads one request
func runEntryTestChild() {
	switch os.Getenv(entryTestChildEnv) {
	case "":
		return
	case "private-rpc-stdin":
		closeRPCStdinOnExec()
		seen := "TIMEOUT"
		if exe, err := os.Executable(); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			child := exec.CommandContext(ctx, exe)
			child.Env = entryChildEnv("stdin-reader")
			child.Stdin = os.Stdin
			if out, err := child.Output(); ctx.Err() == nil && err == nil {
				seen = string(out)
			}
			cancel()
		}
		fmt.Println("child saw:", seen)
		line, _ := bufio.NewReader(entryStdin).ReadString('\n')
		fmt.Println("gateway read:", pyrepr.Repr(line))
	case "stdin-reader":
		raw, _ := io.ReadAll(os.Stdin)
		fmt.Print(pyrepr.Repr(string(raw)))
	}
	os.Exit(0)
}

// Next output line of the fixture that starts with prefix, failing the case at the deadline
func nextFixtureLine(t *testing.T, lines <-chan string, prefix string, transcript *[]string, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case line, open := <-lines:
			if !open {
				t.Fatalf("no %q line from the gateway:\n%s", prefix, strings.Join(*transcript, ""))
			}
			*transcript = append(*transcript, line)
			if strings.HasPrefix(line, prefix) {
				return strings.TrimRight(line, "\r\n")
			}
		case <-deadline:
			t.Fatalf("no %q line from the gateway:\n%s", prefix, strings.Join(*transcript, ""))
		}
	}
}

func TestChildWithoutStdinSeesNulWhileTheRpcPipeStaysOpen(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	gateway := exec.Command(exe)
	gateway.Dir = t.TempDir()
	gateway.Env = append(entryChildEnv("private-rpc-stdin"), "ICEBATH_HOME="+gateway.Dir+`\home`)
	stdin, err := gateway.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	gateway.Stdout, gateway.Stderr = writer, writer
	if err := gateway.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if gateway.ProcessState == nil {
			_ = gateway.Process.Kill()
			_ = gateway.Wait()
		}
	}()
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		scanner := bufio.NewReader(reader)
		for {
			line, err := scanner.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if err != nil {
				return
			}
		}
	}()
	var transcript []string
	if saw := nextFixtureLine(t, lines, "child saw:", &transcript, 60*time.Second); saw != "child saw: ''" {
		t.Fatalf("a child without its own stdin held the RPC pipe:\n%s", strings.Join(transcript, ""))
	}
	if _, err := io.WriteString(stdin, `{"id": "req-1"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if read := nextFixtureLine(t, lines, "gateway read:", &transcript, 30*time.Second); read != `gateway read: '{"id": "req-1"}\n'` {
		t.Fatalf("the gateway lost its request:\n%s", strings.Join(transcript, ""))
	}
	stdin.Close()
	err = gateway.Wait()
	writer.Close()
	if err != nil {
		t.Fatalf("gateway exit = %v\n%s", err, strings.Join(transcript, ""))
	}
}
