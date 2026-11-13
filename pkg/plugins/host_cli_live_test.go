package plugins

import (
	"bytes"
	"io"
	"os"
	"testing"
)

const cliPlugin = `
import argparse, sys

def _greet(args):
    for _ in range(args.times):
        print(("HELLO " if args.shout else "hello ") + args.name + " via " + args.command)
    return 3

def _root(args):
    print("root handler", args.command, sorted(k for k in vars(args) if k != "func"))
    return None

def _setup(parser):
    parser.add_argument("--color", choices=["red", "blue"], default="red")
    sub = parser.add_subparsers(dest="hello_command")
    greet = sub.add_parser("greet", help="Greet someone")
    greet.add_argument("name")
    greet.add_argument("--times", type=int, default=1)
    greet.add_argument("--shout", action="store_true")
    greet.set_defaults(func=_greet)
    fail = sub.add_parser("fail")
    fail.set_defaults(func=lambda args: sys.exit("boom exit"))

def register(ctx):
    ctx.register_cli_command(name="hello", help="Say hello", setup_fn=_setup, handler_fn=_root, description="Hello tool")
`

const brokenCLIPlugin = `
def _setup(parser):
    parser.add_argument("--early")
    raise RuntimeError("setup exploded")

def register(ctx):
    ctx.register_cli_command("broken", "Broken", _setup, lambda args: 0)
`

// Output the child wrote to the terminal streams the host was started with during start
func captureTerminal(t *testing.T, start func()) (stdout func() string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousOut, previousErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	start()
	os.Stdout, os.Stderr = previousOut, previousErr
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	return func() string {
		_ = w.Close()
		<-done
		return buf.String()
	}
}
