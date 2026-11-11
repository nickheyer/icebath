package gateway

import (
	"fmt"
	"log/slog"
	"sync"
)

// What a loop exception handler receives, the asyncio handler context of a failed callback or task
type LoopExceptionContext struct {
	Message   string
	Exception error
	// Name of the task or callback that failed, empty when unknown
	Task string
	// Python repr of the failed callback and of its handle, empty when the failure names none
	Callback, Handle string
}

// Handler a loop routes every callback failure to, the asyncio loop exception handler
type LoopExceptionHandler func(loop *CallbackLoop, c LoopExceptionContext)

var loopExceptionHandlers sync.Map

// Installs handler for every later failure on the loop, nil restoring the default
func (l *CallbackLoop) SetExceptionHandler(handler LoopExceptionHandler) {
	if handler == nil {
		loopExceptionHandlers.Delete(l)
		return
	}
	loopExceptionHandlers.Store(l, handler)
}

// Handler installed on the loop, nil while the default one reports failures
func (l *CallbackLoop) ExceptionHandler() LoopExceptionHandler {
	if handler, ok := loopExceptionHandlers.Load(l); ok {
		return handler.(LoopExceptionHandler)
	}
	return nil
}

// Logs the failure at error level the way asyncio's default handler reports it
func (l *CallbackLoop) DefaultExceptionHandler(c LoopExceptionContext) {
	if c.Exception == nil {
		slog.Error(c.Message)
		return
	}
	slog.Error(fmt.Sprintf("%s: %v", c.Message, c.Exception))
}

// Routes a failure to the installed handler, the default one when none is installed
func (l *CallbackLoop) CallExceptionHandler(c LoopExceptionContext) {
	if handler, ok := loopExceptionHandlers.Load(l); ok {
		handler.(LoopExceptionHandler)(l, c)
		return
	}
	l.DefaultExceptionHandler(c)
}
