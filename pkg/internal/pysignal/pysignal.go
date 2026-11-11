package pysignal

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
)

// Exception the default SIGINT handler raises
var ErrKeyboardInterrupt = errors.New("KeyboardInterrupt")

// Exit a handler asks for, the carrier of a Python SystemExit with an integer status
type SystemExit struct{ Code int }

func (e *SystemExit) Error() string { return fmt.Sprintf("SystemExit: %d", e.Code) }

// Value of one signal slot, the default action, the ignore action or a callable
type Handler struct {
	fn     func(sig os.Signal) error
	ignore bool
}

var (
	Default    = &Handler{}
	Ignore     = &Handler{ignore: true}
	DefaultInt = &Handler{fn: func(os.Signal) error { return ErrKeyboardInterrupt }}
)

// Callable slot value, its error standing for the exception the handler raises
func Func(fn func(sig os.Signal) error) *Handler { return &Handler{fn: fn} }

// Whether the slot value is a callable, false for the default and ignore actions
func (h *Handler) Callable() bool { return h != nil && h.fn != nil }

// Runs a callable slot value and returns what it raised
func (h *Handler) Call(sig os.Signal) error { return h.fn(sig) }

var (
	mu    sync.Mutex
	slots = map[os.Signal]*Handler{}
	chans = map[os.Signal]chan os.Signal{}
)

// Slot value a signal starts with, ignore when inherited ignored and the interrupt raiser for SIGINT
func initial(sig os.Signal) *Handler {
	if signal.Ignored(sig) {
		return Ignore
	}
	if sig == os.Interrupt {
		return DefaultInt
	}
	return Default
}

func current(sig os.Signal) *Handler {
	if h, ok := slots[sig]; ok {
		return h
	}
	h := initial(sig)
	slots[sig] = h
	return h
}

// Handler in the slot of the signal, the getsignal of the Python signal module
func Getsignal(sig os.Signal) *Handler {
	mu.Lock()
	defer mu.Unlock()
	return current(sig)
}

// Replaces the slot of the signal and returns the value it held, the signal of the Python signal module
func Signal(sig os.Signal, h *Handler) *Handler {
	mu.Lock()
	defer mu.Unlock()
	prev := current(sig)
	slots[sig] = h
	ch, listening := chans[sig]
	switch {
	case h.fn != nil && h != DefaultInt:
		if !listening {
			ch = make(chan os.Signal, 8)
			chans[sig] = ch
			go dispatch(sig, ch)
		}
		signal.Notify(ch, sig)
	case h.ignore:
		if listening {
			signal.Stop(ch)
		}
		signal.Ignore(sig)
	default:
		if listening {
			signal.Stop(ch)
		}
		if prev.ignore {
			signal.Reset(sig)
		}
	}
	return prev
}

// Runs the callable in the slot for every delivery of the signal, ending the process on what it raises
func dispatch(sig os.Signal, ch chan os.Signal) {
	for range ch {
		h := Getsignal(sig)
		if !h.Callable() {
			continue
		}
		if err := h.Call(sig); err != nil {
			unhandled(err)
		}
	}
}

// Ends the process the way the interpreter ends on an exception no frame caught
func unhandled(err error) {
	var exit *SystemExit
	switch {
	case errors.Is(err, ErrKeyboardInterrupt):
		Signal(os.Interrupt, Default)
		dieByInterrupt()
	case errors.As(err, &exit):
		os.Exit(exit.Code)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// Sends the signal to this process through its slot, the raise_signal of the Python signal module
func Raise(sig os.Signal) error {
	h := Getsignal(sig)
	switch {
	case h.Callable():
		return h.Call(sig)
	case h.ignore:
		return nil
	}
	defaultAction(sig)
	return nil
}
