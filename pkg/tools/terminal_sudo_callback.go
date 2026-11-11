package tools

import (
	"context"
	"fmt"
)

// SudoPasswordCallback asks the surface for a sudo password, empty on skip, its address naming the cache scope
type SudoPasswordCallback struct {
	Prompt func(ctx context.Context) string
}

const sudoPasswordSlot = "sudo_password"

type sudoPromptCommandKey struct{}

// Registers the surface's sudo password prompt in the per thread slot, nil clearing it
func SetSudoPasswordCallback(ctx context.Context, cb *SudoPasswordCallback) {
	if cb == nil {
		ThreadLocalSet(ctx, sudoPasswordSlot, nil)
		return
	}
	ThreadLocalSet(ctx, sudoPasswordSlot, cb)
}

// The sudo password prompt of the current thread, nil in headless contexts
func GetSudoPasswordCallback(ctx context.Context) *SudoPasswordCallback {
	cb, _ := ThreadLocalGet(ctx, sudoPasswordSlot).(*SudoPasswordCallback)
	return cb
}

// Cache scope naming the registered prompt, empty when the thread has none
func sudoCallbackScope(ctx context.Context) string {
	if cb := GetSudoPasswordCallback(ctx); cb != nil {
		return fmt.Sprintf("callback:%p", cb)
	}
	return ""
}

// Original command of the sudo prompt being answered, empty outside its callback
func GetSudoPromptCommand(ctx context.Context) string {
	cmd, _ := ctx.Value(sudoPromptCommandKey{}).(string)
	return cmd
}

// Runs the registered prompt for command, outcome error and empty password when it panics
func callSudoPasswordCallback(ctx context.Context, env *Env, cb *SudoPasswordCallback, command string, human *humanInputHandle) (password string) {
	defer func() {
		if r := recover(); r != nil {
			human.finish(env, "error")
			password = ""
		}
	}()
	password = func() string {
		release := humanWaitWindow(env, "")
		defer release()
		return cb.Prompt(context.WithValue(ctx, sudoPromptCommandKey{}, command))
	}()
	if password != "" {
		human.finish(env, "provided")
	} else {
		human.finish(env, "skipped")
	}
	return password
}

// Hands the same sudo prompt to propagated worker threads so its cache scope stays one
func init() {
	RegisterThreadCallback(ThreadCallback{
		Get: func(ctx context.Context) any {
			if cb := GetSudoPasswordCallback(ctx); cb != nil {
				return cb
			}
			return nil
		},
		Set: func(ctx context.Context, cb any) { v, _ := cb.(*SudoPasswordCallback); SetSudoPasswordCallback(ctx, v) }})
}

// Context sharing the goroutine local storage of from, one thread running its next task
func WithThreadLocalsOf(ctx, from context.Context) context.Context {
	tl, _ := from.Value(threadLocalsKey{}).(*threadLocals)
	if tl == nil {
		return ctx
	}
	return context.WithValue(ctx, threadLocalsKey{}, tl)
}
