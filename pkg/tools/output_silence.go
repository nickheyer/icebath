package tools

import "context"

type outputSilenceKey struct{}

// Context under which the work it carries prints no tool rows, spinners or turn notices
func SilenceOutput(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, outputSilenceKey{}, true)
}

// Reports whether work under ctx runs with its terminal output silenced
func OutputSilenced(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(outputSilenceKey{}).(bool)
	return v
}

// Executor host view whose terminal prints and spinners do nothing
type silencedExecutorHost struct{ ExecutorHost }

func (silencedExecutorHost) Print(string)                      {}
func (silencedExecutorHost) VPrint(string, bool)               {}
func (silencedExecutorHost) ShouldEmitQuietToolMessages() bool { return false }
func (silencedExecutorHost) ShouldStartQuietSpinner() bool     { return false }
func (silencedExecutorHost) StartSpinner(string) Spinner       { return silentSpinner{} }

// Spinner that shows nothing
type silentSpinner struct{}

func (silentSpinner) Stop(string) {}

// Host the executor prints through for work under ctx, muted inside a silenced context
func (e *Executor) hostFor(ctx context.Context) ExecutorHost {
	if OutputSilenced(ctx) {
		return silencedExecutorHost{e.Host}
	}
	return e.Host
}

// Turn validation host view whose prints do nothing
type silencedValidationHost struct{ TurnValidationHost }

func (silencedValidationHost) Print(string, bool) {}
func (silencedValidationHost) BufferPrint(string) {}

// Turn validation host for work under ctx, muted inside a silenced context
func validationHostFor(ctx context.Context, host TurnValidationHost) TurnValidationHost {
	if OutputSilenced(ctx) {
		return silencedValidationHost{host}
	}
	return host
}
