package tuiserver

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Python class name and str of an unhandled failure, a runtime panic naming its Go error
func crashFailure(failure any) (string, string) {
	err, isErr := failure.(error)
	if !isErr {
		err = errors.New(pyrepr.Str(failure))
	}
	return pyrepr.ExceptionName(err), pyrepr.ExceptionMessage(err)
}

// Appends an unhandled failure with its stack to the crash log and prints its first line on stderr
func recordCrash(kind string, failure any, threadName *string) {
	typeName, message := crashFailure(failure)
	suffix := ""
	if threadName != nil {
		suffix = " · thread=" + *threadName
	}
	stack := debug.Stack()
	appendCrashLog(fmt.Sprintf("%s · %s%s", kind, time.Now().Format("2006-01-02 15:04:05"), suffix), func(w io.Writer) {
		fmt.Fprintf(w, "%s: %s\n%s", typeName, message, stack)
	})
	first := typeName
	if trimmed := strings.TrimSpace(message); trimmed != "" {
		first = strings.SplitN(trimmed, "\n", 2)[0]
	}
	who := ""
	if threadName != nil {
		who = "thread " + *threadName + " raised "
	}
	fmt.Fprintf(os.Stderr, "[gateway-crash] %s%s: %s\n", who, typeName, first)
}

// Records a failure that escaped the main goroutine and lets it terminate the process as the default hook does
func panicHook() {
	if failure := recover(); failure != nil {
		recordCrash("unhandled exception", failure, nil)
		panic(failure)
	}
}

// Ends a goroutine whose body raised the way the thread exception hook does, the crash recorded and the process kept
func threadExceptHook(name string) {
	if failure := recover(); failure != nil {
		recordCrash("thread exception", failure, &name)
	}
}
