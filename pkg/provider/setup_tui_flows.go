package provider

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/setupflow"
)

// Runs an icebath setup flow with its questions put to a host, reporting the model it leaves and a stop
func RunHostedSetupFlow(ctx context.Context, ask SetupAsker, out io.Writer, run func(ctx context.Context, ask setupflow.Asker) error) (HostedModelResult, bool, error) {
	h := home.Dir(ctx)
	before, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		return HostedModelResult{}, false, err
	}
	asking := func(p HostedPrompt) HostedAnswer {
		if ctx.Err() != nil {
			panic(SetupExit{Code: 1})
		}
		answer := ask(p)
		if ctx.Err() != nil {
			panic(SetupExit{Code: 1})
		}
		return answer
	}
	stopped := false
	err = HostSetupFlow(asking, out, func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				exit, isExit := r.(SetupExit)
				if !isExit {
					panic(r)
				}
				stopped, err = exit.Code != 0, nil
			}
		}()
		err = run(ctx, hostedFlowAsker{ask: asking})
		if code, message, exit := SetupSystemExit(err); exit {
			if message != "" {
				fmt.Fprintln(SetupOutput(), message)
			}
			stopped, err = code != 0, nil
		}
		return err
	})
	if err != nil {
		return HostedModelResult{}, false, err
	}
	after, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		return HostedModelResult{}, false, err
	}
	route := modelRoute(after)
	return HostedModelResult{Model: route[0], Provider: route[1], Changed: route != modelRoute(before)}, stopped || ctx.Err() != nil, nil
}

// Flow engine asker over the host of a running setup flow, for the flows written against the flow engine
type hostedFlowAsker struct{ ask SetupAsker }

func (a hostedFlowAsker) Say(line string) { fmt.Fprintln(SetupOutput(), line) }

func (a hostedFlowAsker) Choose(question string, options []setupflow.Option, def int) (int, error) {
	rows := make([]string, len(options))
	for i, o := range options {
		rows[i] = hostedRowText(o.Title, o.Detail)
	}
	answer := a.ask(HostedPrompt{Kind: HostedChoice, Title: question, Rows: rows, Default: def})
	if answer.Cancelled || answer.Index < 0 || answer.Index >= len(options) {
		return def, nil
	}
	return answer.Index, nil
}

func (a hostedFlowAsker) Check(question string, items []setupflow.Item, selected map[int]bool, tokens map[string]int) (map[int]bool, error) {
	rows := make([]string, len(items))
	for i, item := range items {
		detail := item.Detail
		if weight := toolTokens(item.Tools, tokens); weight > 0 {
			detail = hostedRowText(detail, tokenText(weight))
		}
		rows[i] = hostedRowText(item.Title, detail)
	}
	answer := a.ask(HostedPrompt{Kind: HostedChecklist, Title: question, Rows: rows, Picked: setIndices(selected)})
	out := map[int]bool{}
	if answer.Cancelled {
		for i, on := range selected {
			out[i] = on
		}
		return out, nil
	}
	for _, i := range answer.Picked {
		if i >= 0 && i < len(items) {
			out[i] = true
		}
	}
	return out, nil
}

func (a hostedFlowAsker) Text(question, def string, secret bool) (string, error) {
	kind, title := HostedText, question
	if secret {
		kind = HostedSecret
	}
	if def != "" && !secret {
		title = question + " [" + def + "]"
	}
	answer := a.ask(HostedPrompt{Kind: kind, Title: title})
	switch {
	case answer.Cancelled:
		return "", setupflow.ErrCancelled
	case answer.Text == "":
		return def, nil
	}
	return answer.Text, nil
}

// Row of a hosted list, the detail set apart by the column gap the host splits rows on
func hostedRowText(title, detail string) string {
	if detail == "" {
		return title
	}
	if title == "" {
		return detail
	}
	return title + "  " + detail
}

// Context estimate of the distinct tools a row brings
func toolTokens(tools []string, tokens map[string]int) int {
	seen, total := map[string]bool{}, 0
	for _, name := range tools {
		if !seen[name] {
			seen[name] = true
			total += tokens[name]
		}
	}
	return total
}

// Context estimate worded as the tools flow words its total, thousands with one decimal
func tokenText(total int) string {
	if total >= 1000 {
		return "Est. tool context ~" + strconv.FormatFloat(float64(total)/1000, 'f', 1, 64) + "k tokens"
	}
	return "Est. tool context ~" + strconv.Itoa(total) + " tokens"
}
