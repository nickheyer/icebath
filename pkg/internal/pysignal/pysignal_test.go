//go:build !windows

package pysignal

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestSignalReplacesTheSlotAndReturnsThePreviousValue(t *testing.T) {
	seen := make(chan string, 4)
	first := Func(func(os.Signal) error { seen <- "first"; return nil })
	second := Func(func(os.Signal) error { seen <- "second"; return nil })
	original := Signal(syscall.SIGUSR2, first)
	defer Signal(syscall.SIGUSR2, original)
	if original != Default {
		t.Fatalf("a fresh slot must hold the default action, got %v", original)
	}
	if prev := Signal(syscall.SIGUSR2, second); prev != first {
		t.Fatal("replacing a slot must return the handler it held")
	}
	if Getsignal(syscall.SIGUSR2) != second {
		t.Fatal("the slot must hold the last handler set")
	}
	if err := KillSelf(syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-seen:
		if got != "second" {
			t.Fatalf("delivery reached %q, want the handler in the slot", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a delivered signal must run the handler in the slot")
	}
	select {
	case got := <-seen:
		t.Fatalf("a replaced handler still ran: %q", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRaiseRunsTheHandlerBeforeItReturnsAndCarriesItsError(t *testing.T) {
	boom := errors.New("boom")
	ran := false
	original := Signal(syscall.SIGUSR1, Func(func(os.Signal) error { ran = true; return boom }))
	defer Signal(syscall.SIGUSR1, original)
	if err := Raise(syscall.SIGUSR1); err != boom || !ran {
		t.Fatalf("raise returned %v with ran=%v", err, ran)
	}
	Signal(syscall.SIGUSR1, Ignore)
	if err := Raise(syscall.SIGUSR1); err != nil {
		t.Fatalf("an ignored signal raised %v", err)
	}
}

func TestInterruptSlotStartsWithTheKeyboardInterruptRaiser(t *testing.T) {
	if h := initial(os.Interrupt); h != DefaultInt && h != Ignore {
		t.Fatalf("SIGINT starts with %v", h)
	}
	if !errors.Is(DefaultInt.Call(os.Interrupt), ErrKeyboardInterrupt) {
		t.Fatal("the default SIGINT handler must raise KeyboardInterrupt")
	}
}
