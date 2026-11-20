package cliui

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// How one list prompt maps its ending to a result, off a terminal and when replayed
type listSpec struct {
	cancelValue any
	picked      func() any
	fallback    func(ctx context.Context) (any, error)
	replayName  func(value any) string
}

// Runs a list with scoped navigation events, a replayed answer, the live form or the numbered form
func (c *Console) runList(ctx context.Context, p *listPrompt, m listSpec) (any, error) {
	handler := navHandler(ctx)
	notify := func(event MenuNavigationEvent, value ...any) error {
		if handler == nil {
			return nil
		}
		_, err := handler(event, value...)
		return err
	}
	var start *MenuNavigationStart
	if handler != nil {
		s, err := handler(NavEventBegin, p.title)
		if err != nil {
			return nil, err
		}
		start = s
	}
	if start != nil && start.Replay {
		if err := notify(NavEventResolve, start.ReplayValue); err != nil {
			return nil, err
		}
		c.replayedAnswer(p.title, m.replayName(start.ReplayValue))
		return start.ReplayValue, nil
	}
	if !c.stdinTTY() {
		return m.cancelValue, nil
	}
	if !c.interactive() {
		result, err := m.fallback(ctx)
		if err != nil {
			return nil, err
		}
		return result, notify(NavEventResolve, result)
	}
	p.allowBack = start != nil && start.AllowBack
	p.guardEsc = start != nil && start.GuardCancel
	outcome, err := p.run()
	if err != nil {
		return nil, err
	}
	result := m.cancelValue
	switch outcome {
	case listPicked:
		result = m.picked()
	case listBack:
		if err := notify(NavEventBack); err != nil {
			return nil, err
		}
	default:
		if err := notify(NavEventCancel); err != nil {
			return nil, err
		}
	}
	return result, notify(NavEventResolve, result)
}

// Prints the answer line of a prompt whose answer was replayed instead of asked
func (c *Console) replayedAnswer(title, answer string) {
	if !c.interactive() {
		return
	}
	region := c.openRegion()
	region.close(c.answerLines(title, answer, textStyle{}, ""))
}

// Name the answer line gives a replayed choice index, cancelled when it is none of the rows
func replayedRowName(p *listPrompt) func(any) string {
	return func(value any) string {
		if index, ok := value.(int); ok {
			for _, row := range p.rows {
				if row.index == index && index >= 0 {
					return p.rowName(row)
				}
			}
		}
		return "cancelled"
	}
}

// List prompt over items with the cursor on the selected one
func (c *Console) newListPrompt(title string, items []RadioItem, selected int, headings map[int]string) *listPrompt {
	p := &listPrompt{c: c, title: cleanTitle(title), rows: buildListRows(items, headings), checked: map[int]bool{}}
	p.cursor = -1
	for i, row := range p.rows {
		if row.index < 0 {
			continue
		}
		if p.cursor < 0 || row.index == selected {
			p.cursor = i
		}
	}
	p.cursor = max(p.cursor, 0)
	return p
}

// Search text per row, the given labels or the plain row text, none for a heading
func rowLabels(rows []listRow, labels []string) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		switch {
		case row.index < 0:
		case row.index < len(labels):
			out[i] = labels[row.index]
		default:
			out[i] = RadioItemPlain(row.item)
		}
	}
	return out
}

// RadiolistOptions are the optional arguments of a radiolist, nil CancelReturns meaning the selection
type RadiolistOptions struct {
	CancelReturns *int
	Description   string
	Searchable    bool
	SearchLabels  []string
	Headings      map[int]string
}

// CursesRadiolist is a single choice list returning the original index of the chosen row
func (c *Console) CursesRadiolist(ctx context.Context, title string, items []RadioItem, selected int, opts RadiolistOptions) (int, error) {
	cancel := selected
	if opts.CancelReturns != nil {
		cancel = *opts.CancelReturns
	}
	p := c.newListPrompt(title, items, selected, opts.Headings)
	p.desc = pystr.Splitlines(opts.Description, false)
	p.searchable = opts.Searchable
	p.labels = rowLabels(p.rows, opts.SearchLabels)
	p.yesNo = len(items) == 2 && RadioItemPlain(items[0]) == "Yes" && RadioItemPlain(items[1]) == "No"
	res, err := c.runList(ctx, p, listSpec{
		cancelValue: cancel,
		picked:      func() any { return p.rows[p.cursor].index },
		replayName:  replayedRowName(p),
		fallback: func(ctx context.Context) (any, error) {
			return c.radioNumberedFallback(ctx, title, items, selected, cancel, opts.Headings)
		},
	})
	if err != nil {
		return 0, err
	}
	n, ok := res.(int)
	if !ok {
		return 0, fmt.Errorf("menu navigation replay value %T is not a row index", res)
	}
	return n, nil
}

// CursesChecklist is a multiple choice list, cancel returning cancelReturns or the original selection
func (c *Console) CursesChecklist(ctx context.Context, title string, items []string, selected, cancelReturns map[int]bool, statusFn func(map[int]bool) string) (map[int]bool, error) {
	if cancelReturns == nil {
		cancelReturns = copySet(selected)
	}
	rows := make([]RadioItem, len(items))
	for i, label := range items {
		rows[i] = PlainItem(label)
	}
	p := c.newListPrompt(title, rows, 0, nil)
	p.multi, p.checked, p.status = true, copySet(selected), statusFn
	p.labels = rowLabels(p.rows, items)
	res, err := c.runList(ctx, p, listSpec{
		cancelValue: cancelReturns,
		picked:      func() any { return copySet(p.checked) },
		replayName: func(value any) string {
			set, _ := value.(map[int]bool)
			var names []string
			for _, row := range p.rows {
				if set[row.index] {
					names = append(names, p.rowName(row))
				}
			}
			if len(names) == 0 {
				return "none"
			}
			return strings.Join(names, ", ")
		},
		fallback: func(ctx context.Context) (any, error) {
			return c.numberedFallback(ctx, title, items, selected, cancelReturns, statusFn)
		},
	})
	if err != nil {
		return nil, err
	}
	set, ok := res.(map[int]bool)
	if !ok {
		return nil, fmt.Errorf("menu navigation replay value %T is not a checklist selection", res)
	}
	return set, nil
}

// CursesSingleSelect picks one item or the cancel row, ok false on cancel
func (c *Console) CursesSingleSelect(ctx context.Context, title string, items []string, defaultIndex int, cancelLabel string, searchable bool) (int, bool, error) {
	all := append(append([]string{}, items...), cancelLabel)
	rows := make([]RadioItem, len(all))
	for i, label := range all {
		rows[i] = PlainItem(label)
	}
	p := c.newListPrompt(title, rows, min(defaultIndex, len(all)-1), nil)
	p.searchable = searchable
	p.labels = rowLabels(p.rows, all)
	res, err := c.runList(ctx, p, listSpec{
		cancelValue: nil,
		picked: func() any {
			if index := p.rows[p.cursor].index; index < len(items) {
				return index
			}
			return nil
		},
		replayName: replayedRowName(p),
		fallback: func(ctx context.Context) (any, error) {
			return c.numberedSingleFallback(ctx, title, all, len(items))
		},
	})
	if err != nil {
		return 0, false, err
	}
	if res == nil {
		return 0, false, nil
	}
	n, ok := res.(int)
	if !ok {
		return 0, false, fmt.Errorf("menu navigation replay value %T is not a row index", res)
	}
	return n, true, nil
}
