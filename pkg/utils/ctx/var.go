package ctxutil

import (
	"context"
	"errors"
	"sync/atomic"
)

var (
	// Error of a Reset with a token that already undid its Set
	ErrTokenUsed = errors.New("token already used")
	// Error of a Reset with a token another variable's Set gave
	ErrTokenVariable = errors.New("token belongs to another variable")
	// Error of a Reset through a context running in another store than the Set did
	ErrTokenStore = errors.New("token belongs to another store")
)

// Variable whose value lives in the Store of the context it is used through, reading as its default where that Store holds none
type Var[T any] struct {
	name string
	def  T
}

// Variable with its name and default value
func NewVar[T any](name string, def T) *Var[T] { return &Var[T]{name: name, def: def} }

// Name the variable was declared with
func (v *Var[T]) Name() string { return v.name }

func (v *Var[T]) isVariable() {}

// Value the store ctx runs in holds, else the default
func (v *Var[T]) Get(ctx context.Context) T {
	if val, ok := StoreOf(ctx).lookup(v); ok {
		t, _ := val.(T)
		return t
	}
	return v.def
}

// Stores value in the store ctx runs in and returns the token that undoes it
func (v *Var[T]) Set(ctx context.Context, value T) *Token {
	s := StoreOf(ctx)
	old, had := s.swap(v, value, true)
	return &Token{v: v, store: s, old: old, had: had}
}

// Undoes the Set that gave t, putting back the value held before or none, through a context running in the same store
func (v *Var[T]) Reset(ctx context.Context, t *Token) error {
	if t.used.Load() {
		return ErrTokenUsed
	}
	if t.v != Variable(v) {
		return ErrTokenVariable
	}
	if t.store != StoreOf(ctx) {
		return ErrTokenStore
	}
	if !t.used.CompareAndSwap(false, true) {
		return ErrTokenUsed
	}
	t.store.swap(v, t.old, t.had)
	return nil
}

// Record of one Set that Reset uses to put the earlier value back once
type Token struct {
	v     Variable
	store *Store
	old   any
	had   bool
	used  atomic.Bool
}
