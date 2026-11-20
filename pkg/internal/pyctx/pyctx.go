package pyctx

import (
	"context"
	"errors"
	"sync"
)

// Variable whose value lives in a Context, read back as its default where the Context never set it
type Var struct {
	name string
	def  any
}

// Declares a variable with its default value
func NewVar(name string, def any) *Var { return &Var{name: name, def: def} }

// Name the variable was declared with
func (v *Var) Name() string { return v.name }

// Mutable mapping of variables to values that one logical thread of execution runs inside
type Context struct {
	mu   sync.RWMutex
	vals map[*Var]any
	keys []*Var
}

// Empty context like contextvars.Context()
func New() *Context { return &Context{vals: map[*Var]any{}} }

// Independent shallow copy holding the same values
func (c *Context) Copy() *Context {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := &Context{vals: make(map[*Var]any, len(c.vals)), keys: append([]*Var(nil), c.keys...)}
	for k, v := range c.vals {
		out.vals[k] = v
	}
	return out
}

// Variable value pair of a context
type Item struct {
	Var   *Var
	Value any
}

// Every set variable with its value in first set order
func (c *Context) Items() []Item {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Item, 0, len(c.keys))
	for _, k := range c.keys {
		out = append(out, Item{Var: k, Value: c.vals[k]})
	}
	return out
}

func (c *Context) lookup(v *Var) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	val, ok := c.vals[v]
	return val, ok
}

func (c *Context) store(v *Var, val any, present bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, had := c.vals[v]
	if !present {
		if had {
			delete(c.vals, v)
			for i, k := range c.keys {
				if k == v {
					c.keys = append(c.keys[:i], c.keys[i+1:]...)
					break
				}
			}
		}
		return
	}
	if !had {
		c.keys = append(c.keys, v)
	}
	c.vals[v] = val
}

type ctxKey struct{}

// Context of the main thread, used by callers that never entered one
var root = New()

// Runs work under c the way Context.run enters it
func With(ctx context.Context, c *Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// Context the caller runs inside, the main thread context when none was entered
func Current(ctx context.Context) *Context {
	if ctx != nil {
		if c, ok := ctx.Value(ctxKey{}).(*Context); ok && c != nil {
			return c
		}
	}
	return root
}

// Copy of the current context like contextvars.copy_context
func CopyContext(ctx context.Context) *Context { return Current(ctx).Copy() }

// Value in the current context, else the default
func (v *Var) Get(ctx context.Context) any {
	if val, ok := Current(ctx).lookup(v); ok {
		return val
	}
	return v.def
}

// Restores a variable to what it held before one Set
type Token struct {
	v       *Var
	c       *Context
	old     any
	present bool
	used    bool
}

// Sets the value in the current context and returns the token that undoes it
func (v *Var) Set(ctx context.Context, value any) *Token {
	c := Current(ctx)
	old, present := c.lookup(v)
	c.store(v, value, true)
	return &Token{v: v, c: c, old: old, present: present}
}

// Undoes the Set that produced the token, refusing a reused, foreign variable or foreign context token
func (v *Var) Reset(ctx context.Context, t *Token) error {
	if t.used {
		return errors.New("Token has already been used once")
	}
	if t.v != v {
		return errors.New("Token was created by a different ContextVar")
	}
	if t.c != Current(ctx) {
		return errors.New("Token was created in a different Context")
	}
	t.c.store(v, t.old, t.present)
	t.used = true
	return nil
}
