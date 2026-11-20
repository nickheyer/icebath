package tui

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Whether the attached gateway reported another build or contract version than this process runs
var gatewayOtherBuild atomic.Bool

// Text of an answer that does not fit its contract, naming the other gateway build when that is the cause
func misfitText(text string) string {
	if gatewayOtherBuild.Load() {
		return tr("gatewayMsg.otherBuild.misfit", text)
	}
	return text
}

// Printing request wrapper of an app part, its failure reported and nil handed on
type rpcVia func(method string, params map[string]any, then func(map[string]any) tea.Cmd) tea.Cmd

// Typed request through a printing wrapper, the decoded answer handed on or nil after a printed misfit
func ask[P, R any](via rpcVia, sys func(string), m tuigateway.Method[P, R], params P, then func(*R) tea.Cmd) tea.Cmd {
	return via(m.Name(), wireParams(params), func(res map[string]any) tea.Cmd {
		if res == nil {
			return then(nil)
		}
		r, err := m.Result(res)
		if err != nil {
			sys("error: " + misfitText(tr("session.common.invalidResponse", m.Name())) + " " + err.Error())
			return then(nil)
		}
		return then(&r)
	})
}

// Typed request whose decoded answer or failure continues on the update loop
func call[P, R any](gw turnGateway, m tuigateway.Method[P, R], params P, then func(*R, error) tea.Cmd) tea.Cmd {
	return callWithin(gw, m, params, 0, then)
}

// Typed request given its own wait, the client's default for zero, whose decoded answer or failure continues on the update loop
func callWithin[P, R any](gw turnGateway, m tuigateway.Method[P, R], params P, timeout time.Duration, then func(*R, error) tea.Cmd) tea.Cmd {
	return gatewayCallWithin(gw, m.Name(), wireParams(params), timeout, func(v any, err error) tea.Cmd {
		if err != nil {
			return then(nil, err)
		}
		r, err := m.Result(v)
		if err != nil {
			return then(nil, fmt.Errorf("%s %w", misfitText(tr("session.common.invalidResponse", m.Name())), err))
		}
		return then(&r, nil)
	})
}

// Typed request whose answer and failure nothing reads
func fire[P, R any](gw turnGateway, m tuigateway.Method[P, R], params P) tea.Cmd {
	return call(gw, m, params, func(*R, error) tea.Cmd { return nil })
}

// Typed request on the caller's goroutine, nil when it failed or its answer did not fit
func quietAsk[P, R any](gw turnGateway, m tuigateway.Method[P, R], params P) *R {
	raw, err := gw.request(context.Background(), m.Name(), wireParams(params), 0)
	if err != nil {
		return nil
	}
	r, err := m.Result(raw)
	if err != nil {
		return nil
	}
	return &r
}

// Pointer to a text, nil for an empty one
func optionalText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Params of a typed request as the plain object the link sends
func wireParams(params any) map[string]any {
	plain, _ := tuigateway.Plain(params).(map[string]any)
	if plain == nil {
		plain = map[string]any{}
	}
	return plain
}

// Continuation of a typed request whose answer nothing reads
func done[R any](*R) tea.Cmd { return nil }

// Pointer to a value, for the optional fields of a typed request
func ptr[T any](v T) *T { return &v }

// Value a pointer holds, the zero value for nil
func val[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
