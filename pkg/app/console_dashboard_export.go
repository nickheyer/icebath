package app

import (
	"context"
	"sync"
)

// Console engine of one dashboard socket, each line run under the context its caller scoped to a profile
func NewDashboardConsole(ctx context.Context, outputLimit int) func(ctx context.Context, line string, confirmed bool) (status, output, command, confirmation string, err error) {
	engine := NewConsoleEngine(ctx, outputLimit)
	var mu sync.Mutex
	return func(ctx context.Context, line string, confirmed bool) (string, string, string, string, error) {
		mu.Lock()
		scoped := *engine
		scoped.History = append([]string(nil), engine.History...)
		seen := len(scoped.History)
		mu.Unlock()
		scoped.ctx = ctx
		result, err := scoped.Execute(line, confirmed)
		mu.Lock()
		engine.History = append(engine.History, scoped.History[seen:]...)
		mu.Unlock()
		return string(result.Status), result.Output, result.Command, result.ConfirmationMessage, err
	}
}
