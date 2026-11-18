package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

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
			sys("error: " + tr("session.common.invalidResponse", m.Name()) + " " + err.Error())
			return then(nil)
		}
		return then(&r)
	})
}

// Typed request whose decoded answer or failure continues on the update loop
func call[P, R any](gw turnGateway, m tuigateway.Method[P, R], params P, then func(*R, error) tea.Cmd) tea.Cmd {
	return gatewayCall(gw, m.Name(), wireParams(params), func(v any, err error) tea.Cmd {
		if err != nil {
			return then(nil, err)
		}
		r, err := m.Result(v)
		if err != nil {
			return then(nil, fmt.Errorf("%s %w", tr("session.common.invalidResponse", m.Name()), err))
		}
		return then(&r, nil)
	})
}

// Params of a typed request as the plain object the link sends
func wireParams(params any) map[string]any {
	plain, _ := tuigateway.Plain(params).(map[string]any)
	if plain == nil {
		plain = map[string]any{}
	}
	return plain
}

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
