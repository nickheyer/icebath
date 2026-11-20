//go:build !windows

package tuiserver

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// Names the stdin boundary fixture a re-executed test binary runs in place of the tests
const entryTestChildEnv = "ICEBATH_TEST_ENTRY_CHILD"

// Environment of this process with the fixture mode replaced by mode
func entryChildEnv(mode string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, entryTestChildEnv+"=") {
			env = append(env, kv)
		}
	}
	return append(env, entryTestChildEnv+"="+mode)
}

// Marks the RPC stdin close on exec and replaces the image, or reports whether the image reads RPC bytes
func runEntryTestChild() {
	switch os.Getenv(entryTestChildEnv) {
	case "":
		return
	case "rpc-stdin-cloexec":
		closeRPCStdinOnExec()
		exe, err := os.Executable()
		if err == nil {
			err = syscall.Exec(exe, []string{exe}, entryChildEnv("read-stdin-probe"))
		}
		fmt.Println(err)
		os.Exit(3)
	case "read-stdin-probe":
		buf := make([]byte, 1)
		if n, err := syscall.Read(0, buf); err == nil && n == 1 {
			fmt.Println("stdin-inherited")
		} else {
			fmt.Println("stdin-closed")
		}
	}
	os.Exit(0)
}

func TestRpcStdinIsClosedBeforeABareChildExecutes(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if _, err := writer.WriteString(`{"method":"prompt.submit"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe)
	child.Env = entryChildEnv("rpc-stdin-cloexec")
	child.Stdin = reader
	out, err := child.Output()
	if err != nil {
		t.Fatalf("child = %v %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "stdin-closed" {
		t.Fatalf("a child executed without its own stdin read the RPC pipe: %q", out)
	}
}
