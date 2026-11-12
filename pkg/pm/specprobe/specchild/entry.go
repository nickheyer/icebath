package specchild

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
	"github.com/nickheyer/icebath/pkg/pm/specprobe"
)

// Exit statuses of a child refused its arguments, unable to start or finalize, or fed a broken question stream
const (
	exitUsage          = 2
	exitStartFailed    = 3
	exitStreamFailed   = 4
	exitFinalizeFailed = 5
	finalizeDeadline   = 20 * time.Second
)

// Runs the find_spec child when this binary was started as one, exiting with its status
func DispatchEntry() {
	if len(os.Args) > 1 && os.Args[1] == specprobe.Entry {
		os.Exit(childMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
}

// Hosts the named interpreter with its venv honoured and answers one JSON module name per stdin line
func childMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "--python" {
		fmt.Fprintf(stderr, "%s: usage: %s --python <interpreter>\n", specprobe.Entry, specprobe.Entry)
		return exitUsage
	}
	in, err := pyhost.Discover(args[1])
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", specprobe.Entry, err)
		return exitStartFailed
	}
	h, err := pyhost.Start(pyhost.Options{Interpreter: in, Argv: []string{args[1]}})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", specprobe.Entry, err)
		return exitStartFailed
	}
	code := serve(h, stdin, stdout, stderr)
	rc, err := h.Finalize(finalizeDeadline)
	if (err != nil || rc != 0) && code == 0 {
		fmt.Fprintf(stderr, "%s: Py_FinalizeEx returned %d: %v\n", specprobe.Entry, rc, err)
		return exitFinalizeFailed
	}
	return code
}

// Answers each question line in order until stdin closes
func serve(h *pyhost.Host, stdin io.Reader, stdout, stderr io.Writer) int {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	encoder := json.NewEncoder(stdout)
	for scanner.Scan() {
		var module string
		if err := json.Unmarshal(scanner.Bytes(), &module); err != nil {
			fmt.Fprintf(stderr, "%s: question is not a JSON string: %v\n", specprobe.Entry, err)
			return exitStreamFailed
		}
		var reply specprobe.Answer
		err := h.Do(func(g *pyhost.GIL) error {
			var err error
			reply, err = findSpec(g, module)
			return err
		})
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", specprobe.Entry, err)
			return exitStreamFailed
		}
		if err := encoder.Encode(reply); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", specprobe.Entry, err)
			return exitStreamFailed
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", specprobe.Entry, err)
		return exitStreamFailed
	}
	return 0
}

// Answers like hermes _importable, a module already in sys.modules counts and ImportError or ValueError read as absent
func findSpec(g *pyhost.GIL, module string) (specprobe.Answer, error) {
	present, err := g.CallMethod(g.SysModules(), "__contains__", []any{module}, nil)
	if err != nil {
		return specprobe.Answer{}, err
	}
	loaded, err := g.Truthy(present)
	g.Decref(present)
	if err != nil {
		return specprobe.Answer{}, err
	}
	if loaded {
		return specprobe.Answer{Found: true}, nil
	}
	util, err := g.Import("importlib.util")
	if err != nil {
		return specprobe.Answer{}, err
	}
	defer g.Decref(util)
	spec, raised := g.CallMethod(util, "find_spec", []any{module}, nil)
	if raised == nil {
		defer g.Decref(spec)
		return specprobe.Answer{Found: !g.IsNone(spec)}, nil
	}
	builtins, err := g.Import("builtins")
	if err != nil {
		return specprobe.Answer{}, err
	}
	defer g.Decref(builtins)
	for _, name := range []string{"ImportError", "ValueError"} {
		cls, err := g.Attr(builtins, name)
		if err != nil {
			return specprobe.Answer{}, err
		}
		matched, err := g.ExceptionMatches(raised, cls)
		g.Decref(cls)
		if err != nil {
			return specprobe.Answer{}, err
		}
		if matched {
			return specprobe.Answer{}, nil
		}
	}
	return specprobe.Answer{Error: raised.Error()}, nil
}
