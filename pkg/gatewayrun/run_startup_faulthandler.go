package gatewayrun

import (
	"os"
	"os/signal"
	"sync"
)

// Registration of the stack dump signal, replaced on each install like a faulthandler register call
var stackDump struct {
	mu   sync.Mutex
	ch   chan os.Signal
	file *os.File
}

// Writes every goroutine stack to f on each stack dump signal without terminating the process
func registerStackDump(f *os.File) {
	stackDump.mu.Lock()
	defer stackDump.mu.Unlock()
	if stackDump.ch != nil {
		signal.Stop(stackDump.ch)
		close(stackDump.ch)
		stackDump.file.Close()
	}
	ch := make(chan os.Signal, 1)
	stackDump.ch, stackDump.file = ch, f
	signal.Notify(ch, stackDumpSignal)
	go func() {
		for range ch {
			stackDump.mu.Lock()
			if stackDump.file == f {
				f.Write(goroutineStacks())
			}
			stackDump.mu.Unlock()
		}
	}()
}
