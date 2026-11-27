package tools

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/strs"
)

// Unit a server's costs count in when it names none
const defaultUnit = "credits"

// Declared spend of one tool call, a range or a single figure
type toolCost struct {
	low, high float64
	note      string
}

// Tool costs a server declares, its spend limit per chat and the unit both count in
type costPolicy struct {
	// By tool name, lower cased since the settings file keeps names so
	costs map[string]toolCost
	limit float64
	unit  string
}

// Non negative finite number, or the error naming where it was declared
func costNumber(v any, where string) (float64, error) {
	var f float64
	switch n := v.(type) {
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case uint64:
		f = float64(n)
	case float64:
		f = n
	default:
		return 0, fmt.Errorf("%s must be a number of 0 or more, not “%v”.", where, v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, fmt.Errorf("%s must be a number of 0 or more, not “%v”.", where, v)
	}
	return f, nil
}

// One declared cost: a number, a [min, max] range, or a block of credits and a note
func parseCost(v any, where string) (toolCost, error) {
	note := ""
	if block, ok := v.(map[string]any); ok {
		_, hasCredits := block["credits"]
		for k := range block {
			if k != "credits" && k != "note" {
				hasCredits = false
			}
		}
		if !hasCredits {
			return toolCost{}, fmt.Errorf("%s must be a number, a range [min, max], or a table of credits and a note.", where)
		}
		if n, present := block["note"]; present {
			text, ok := n.(string)
			if !ok {
				return toolCost{}, fmt.Errorf("%s has a note that is not text.", where)
			}
			note = text
		}
		v = block["credits"]
	}
	if pair, ok := v.([]any); ok {
		if len(pair) != 2 {
			return toolCost{}, fmt.Errorf("%s must be a range of two numbers, [min, max].", where)
		}
		low, err := costNumber(pair[0], where)
		if err != nil {
			return toolCost{}, err
		}
		high, err := costNumber(pair[1], where)
		if err != nil {
			return toolCost{}, err
		}
		if low > high {
			return toolCost{}, fmt.Errorf("%s must be a range [min, max] whose first number is not above the second.", where)
		}
		return toolCost{low: low, high: high, note: strings.TrimSpace(note)}, nil
	}
	amount, err := costNumber(v, where)
	if err != nil {
		return toolCost{}, err
	}
	return toolCost{low: amount, high: amount, note: strings.TrimSpace(note)}, nil
}

// Cost policy of a server's settings, nil when it declares no cost, an error naming the bad key otherwise
func parseCosts(server string, cfg map[string]any) (*costPolicy, error) {
	where := "the MCP server " + server
	policy := &costPolicy{costs: map[string]toolCost{}, unit: defaultUnit}
	if raw := cfg["tool_costs"]; raw != nil {
		table, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tool_costs of %s must be a table of tool names and their costs.", where)
		}
		for tool, v := range table {
			if tool == "" {
				return nil, fmt.Errorf("tool_costs of %s holds a cost for a tool with no name.", where)
			}
			cost, err := parseCost(v, fmt.Sprintf("The cost of %s on %s", tool, where))
			if err != nil {
				return nil, err
			}
			policy.costs[strings.ToLower(tool)] = cost
		}
	}
	if raw := cfg["spend_limit"]; raw != nil {
		limit, err := costNumber(raw, "spend_limit of "+where)
		if err != nil {
			return nil, err
		}
		policy.limit = limit
	}
	if raw := cfg["cost_unit"]; raw != nil {
		unit, ok := raw.(string)
		if !ok || strings.TrimSpace(unit) == "" || len([]rune(unit)) > 32 {
			return nil, fmt.Errorf("cost_unit of %s must be a short word, not “%v”.", where, raw)
		}
		policy.unit = strings.TrimSpace(unit)
	}
	if !slices.ContainsFunc(mapsValues(policy.costs), func(c toolCost) bool { return c.high > 0 }) {
		return nil, nil
	}
	return policy, nil
}

func mapsValues(m map[string]toolCost) []toolCost {
	out := make([]toolCost, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// Names the policy declares that the server does not offer, sorted
func (p *costPolicy) unknown(offered []string) []string {
	if p == nil {
		return nil
	}
	var out []string
	for name := range p.costs {
		if !slices.ContainsFunc(offered, func(tool string) bool { return strings.EqualFold(tool, name) }) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Amount with thousands separators, whole numbers without decimals and fractions to two places
func amount(f float64) string {
	return strs.Number(math.Round(f*100) / 100)
}

// Amount and unit, a plural unit said in the singular for exactly one
func qty(f float64, unit string) string {
	return strs.Counted(math.Round(f*100)/100, unit)
}

// Spend of each chat on each server of a profile, and the slot one prompt about it holds
type tallies struct {
	mu    sync.Mutex
	spent map[[2]string]float64
	asks  map[[2]string]*sync.Mutex
}

// One costed call about to be let through: what it costs and whose tally it lands on
type charge struct {
	tallies *tallies
	key     [2]string
	server  string
	tool    string
	cost    toolCost
	policy  *costPolicy
}

// Charge a call adds, nil for a tool that declares no cost
func (t *tallies) charge(policy *costPolicy, server, tool, session string) *charge {
	if policy == nil {
		return nil
	}
	cost, ok := policy.costs[strings.ToLower(tool)]
	if !ok || cost.high <= 0 {
		return nil
	}
	return &charge{tallies: t, key: [2]string{server, session}, server: server, tool: tool, cost: cost, policy: policy}
}

// Spend the chat booked so far on the server
func (c *charge) spent() float64 {
	c.tallies.mu.Lock()
	defer c.tallies.mu.Unlock()
	return c.tallies.spent[c.key]
}

func (c *charge) over(already float64) bool {
	return c.policy.limit > 0 && already+c.cost.high > c.policy.limit
}

// Books the estimate when it keeps the chat within its limit, whether it did and the spend before
func (c *charge) reserve() (bool, float64) {
	c.tallies.mu.Lock()
	defer c.tallies.mu.Unlock()
	already := c.tallies.spent[c.key]
	if c.over(already) {
		return false, already
	}
	c.tallies.spent[c.key] = already + c.cost.high
	return true, already
}

func (c *charge) book() { c.add(c.cost.high) }

func (c *charge) refund() { c.add(-c.cost.high) }

func (c *charge) add(f float64) {
	c.tallies.mu.Lock()
	defer c.tallies.mu.Unlock()
	c.tallies.spent[c.key] = math.Max(0, c.tallies.spent[c.key]+f)
}

// Holds the chat's prompt slot for the server until the returned release
func (c *charge) asking() func() {
	c.tallies.mu.Lock()
	slot := c.tallies.asks[c.key]
	if slot == nil {
		slot = &sync.Mutex{}
		c.tallies.asks[c.key] = slot
	}
	c.tallies.mu.Unlock()
	slot.Lock()
	return slot.Unlock
}

func (c *charge) costSentence() string {
	unit, cost := c.policy.unit, c.cost
	text := "Costs about " + qty(cost.high, unit)
	if cost.low != cost.high {
		text = "Costs " + amount(cost.low) + " to " + qty(cost.high, unit)
	}
	if note := strings.TrimSpace(cost.note); note != "" {
		if !strings.ContainsAny(note[len(note)-1:], ".!?") {
			note += "."
		}
		return text + ". " + strings.ToUpper(note[:1]) + note[1:]
	}
	return text + "."
}

func (c *charge) spendSentence(already float64) string {
	unit, limit := c.policy.unit, c.policy.limit
	if already > 0 {
		if limit > 0 {
			return fmt.Sprintf("This session has used about %s on %s, of a limit of %s.", qty(already, unit), c.server, amount(limit))
		}
		return fmt.Sprintf("This session has used about %s on %s.", qty(already, unit), c.server)
	}
	if limit > 0 {
		return fmt.Sprintf("The spending limit of this session on %s is %s.", c.server, qty(limit, unit))
	}
	return ""
}

// Cost and spend sentences of a prompt, empty ones left out
func (c *charge) lines(already float64) []string {
	return slices.DeleteFunc([]string{c.costSentence(), c.spendSentence(already)}, func(s string) bool { return s == "" })
}

// Result text of a call refused at the spend limit
func (c *charge) refusal(already float64) string {
	unit := c.policy.unit
	return fmt.Sprintf("MCP tool '%s' on server '%s' was NOT run: this chat has used about %s on '%s' and its spend limit is %s. "+
		"This call (about %s) needed an explicit approval and did not get one. Do not retry; tell the user the spend limit was reached.",
		c.tool, c.server, qty(already, unit), c.server, qty(c.policy.limit, unit), qty(c.cost.high, unit))
}
