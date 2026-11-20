package mcp

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Unit label a cost table counts in when it names none
const DefaultCostUnit = "credits"

// Native mcp_servers keys that declare costs, a portable package's costs maps onto tool_costs
var CostConfigKeys = []string{"tool_costs", "spend_limit", "cost_unit"}

// ToolCost is the declared spend of one tool call, a range or a single figure
type ToolCost struct {
	Low  float64
	High float64
	Note string
}

// CostPolicy is one server's declared tool costs, its per chat spend limit and the unit both count in
type CostPolicy struct {
	Costs map[string]ToolCost
	Limit float64
	Unit  string
}

// Non negative finite number or the error naming where it was declared
func costNumber(value any, where string) (float64, error) {
	var f float64
	switch v := value.(type) {
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	case uint64:
		f = float64(v)
	case float64:
		f = v
	default:
		return 0, fmt.Errorf("%s must be a non-negative number, not %s", where, pyRepr(value))
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, fmt.Errorf("%s must be a non-negative number, not %s", where, pyRepr(value))
	}
	return f, nil
}

// One declared cost, a number, a [min, max] range, or a credits and note block
func parseCost(value any, where string) (ToolCost, error) {
	note := ""
	if m, ok := value.(map[string]any); ok {
		_, hasCredits := m["credits"]
		extra := false
		for k := range m {
			if k != "credits" && k != "note" {
				extra = true
			}
		}
		if !hasCredits || extra {
			return ToolCost{}, fmt.Errorf(`%s must be a number, [min, max], or {"credits": ..., "note": "..."}`, where)
		}
		if n, present := m["note"]; present {
			s, isStr := n.(string)
			if !isStr {
				return ToolCost{}, fmt.Errorf("%s note must be a string", where)
			}
			note = s
		}
		value = m["credits"]
	}
	if list, ok := value.([]any); ok {
		if len(list) != 2 {
			return ToolCost{}, fmt.Errorf("%s range must be [min, max]", where)
		}
		low, err := costNumber(list[0], where)
		if err != nil {
			return ToolCost{}, err
		}
		high, err := costNumber(list[1], where)
		if err != nil {
			return ToolCost{}, err
		}
		if low > high {
			return ToolCost{}, fmt.Errorf("%s range must be [min, max] with min <= max", where)
		}
		return ToolCost{Low: low, High: high, Note: pyStrip(note)}, nil
	}
	amount, err := costNumber(value, where)
	if err != nil {
		return ToolCost{}, err
	}
	return ToolCost{Low: amount, High: amount, Note: pyStrip(note)}, nil
}

// One server's validated cost declaration, nil when it declares no cost, an error naming the bad key otherwise
func ParseCostPolicy(costs, spendLimit, costUnit any, where string) (*CostPolicy, error) {
	table := map[string]ToolCost{}
	if costs != nil {
		m, ok := costs.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s costs must be an object mapping MCP tool names to costs", where)
		}
		for _, tool := range orderedKeys(m) {
			if tool == "" {
				return nil, fmt.Errorf("%s costs keys must be non-empty MCP tool names", where)
			}
			c, err := parseCost(m[tool], fmt.Sprintf("%s cost of '%s'", where, tool))
			if err != nil {
				return nil, err
			}
			table[tool] = c
		}
	}
	limit := 0.0
	if spendLimit != nil {
		l, err := costNumber(spendLimit, where+" spend_limit")
		if err != nil {
			return nil, err
		}
		limit = l
	}
	var unit any = DefaultCostUnit
	if costUnit != nil {
		unit = costUnit
	}
	label, ok := unit.(string)
	if !ok || pyStrip(label) == "" || len([]rune(label)) > 32 {
		return nil, fmt.Errorf("%s cost_unit must be a short non-empty label, not %s", where, pyRepr(costUnit))
	}
	declared := false
	for _, c := range table {
		if c.High > 0 {
			declared = true
		}
	}
	if !declared {
		return nil, nil
	}
	return &CostPolicy{Costs: table, Limit: limit, Unit: pyStrip(label)}, nil
}

// Policy of a native or translated mcp_servers entry, an error when malformed
func ConfigCostPolicy(serverName string, cfg map[string]any) (*CostPolicy, error) {
	return ParseCostPolicy(cfg["tool_costs"], cfg["spend_limit"], cfg["cost_unit"], fmt.Sprintf("MCP server '%s'", serverName))
}

// Amount with thousands separators, whole numbers without decimals and fractions to two places
func fmtAmount(amount float64) string {
	if amount == math.Trunc(amount) {
		return groupThousands(strconv.FormatFloat(math.Trunc(amount), 'f', 0, 64))
	}
	text := strconv.FormatFloat(amount, 'f', 2, 64)
	whole, frac, _ := strings.Cut(text, ".")
	out := groupThousands(whole) + "." + frac
	return strings.TrimRight(strings.TrimRight(out, "0"), ".")
}

// Integer digits grouped by commas the way the comma format spec groups them
func groupThousands(digits string) string {
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	var parts []string
	for len(digits) > 3 {
		parts = append([]string{digits[len(digits)-3:]}, parts...)
		digits = digits[:len(digits)-3]
	}
	parts = append([]string{digits}, parts...)
	return sign + strings.Join(parts, ",")
}

// Amount and unit, a plural unit losing its s for exactly one
func qty(amount float64, unit string) string {
	if amount == 1 && strings.HasSuffix(unit, "s") {
		unit = unit[:len(unit)-1]
	}
	return fmtAmount(amount) + " " + unit
}

// tallyKey is one chat's spend on one policy, the policy key carrying the profile scope under a multiplexer
type tallyKey struct {
	Policy  ServerKey
	Session string
}

var (
	costsMu         sync.Mutex
	spent           = map[tallyKey]float64{}
	askLocks        = map[tallyKey]*sync.Mutex{}
	unscopedMovedTo string
)

type runKeyCtx struct{}

// Binds a cron run id so each run gets its own tally instead of sharing the empty session key
func BindRun(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, runKeyCtx{}, runID)
}

// Run id bound to the context, empty outside a cron run
func runKey(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(runKeyCtx{}).(string)
	return s
}

// Session rotations through the approval yolo transfer also move the chat spend totals
func init() { tools.MCPTransferSession = TransferSession }

// Moves a chat's totals to its new session id so a rotation never hands it a fresh budget
func TransferSession(oldKey, newKey string) {
	if oldKey == "" || newKey == "" || oldKey == newKey {
		return
	}
	costsMu.Lock()
	defer costsMu.Unlock()
	for k, amount := range spent {
		if k.Session != oldKey {
			continue
		}
		moved := tallyKey{Policy: k.Policy, Session: newKey}
		delete(spent, k)
		spent[moved] += amount
	}
}

// Moves bare name totals and prompt slots under the scope a process starts multiplexing with
func rekeyUnscopedCosts(scope string) {
	costsMu.Lock()
	defer costsMu.Unlock()
	unscopedMovedTo = scope
	for k, amount := range spent {
		if k.Policy.Scope != "" {
			continue
		}
		delete(spent, k)
		spent[liveTallyKey(k)] += amount
	}
	for k, slot := range askLocks {
		if k.Policy.Scope != "" {
			continue
		}
		delete(askLocks, k)
		if _, exists := askLocks[liveTallyKey(k)]; !exists {
			askLocks[liveTallyKey(k)] = slot
		}
	}
}

// Tally key a charge books against now, a bare key following its policy once the process multiplexes
func liveTallyKey(k tallyKey) tallyKey {
	if k.Policy.Scope == "" && unscopedMovedTo != "" {
		return tallyKey{Policy: ServerKey{Scope: unscopedMovedTo, Name: k.Policy.Name}, Session: k.Session}
	}
	return k
}

// Charge is one costed call about to be admitted, what it costs and whose tally it lands on
type Charge struct {
	key    tallyKey
	Server string
	Tool   string
	Cost   ToolCost
	Policy *CostPolicy
}

// Spend the chat booked so far on this policy
func (c *Charge) Spent() float64 {
	costsMu.Lock()
	defer costsMu.Unlock()
	return spent[liveTallyKey(c.key)]
}

// Reports whether this call would take the chat past its limit
func (c *Charge) OverLimit(already float64) bool {
	return c.Policy.Limit > 0 && already+c.Cost.High > c.Policy.Limit
}

// Books the estimate when it keeps the chat within its limit, returning whether it booked and the spend before
func (c *Charge) ReserveWithinLimit() (bool, float64) {
	costsMu.Lock()
	defer costsMu.Unlock()
	already := spent[liveTallyKey(c.key)]
	if c.OverLimit(already) {
		return false, already
	}
	c.addLocked(c.Cost.High)
	return true, already
}

// Books the estimate unconditionally
func (c *Charge) Book() {
	costsMu.Lock()
	defer costsMu.Unlock()
	c.addLocked(c.Cost.High)
}

// Returns a booked estimate to the chat
func (c *Charge) Refund() {
	costsMu.Lock()
	defer costsMu.Unlock()
	c.addLocked(-c.Cost.High)
}

// Adds to the tally, floored at zero, caller holds the costs lock
func (c *Charge) addLocked(amount float64) {
	k := liveTallyKey(c.key)
	spent[k] = math.Max(0, spent[k]+amount)
}

// Holds this chat's budget prompt slot until the returned release runs
func (c *Charge) Asking() func() {
	costsMu.Lock()
	k := liveTallyKey(c.key)
	slot := askLocks[k]
	if slot == nil {
		slot = &sync.Mutex{}
		askLocks[k] = slot
	}
	costsMu.Unlock()
	slot.Lock()
	return slot.Unlock
}

// Sentence naming the call's declared cost
func (c *Charge) CostSentence() string {
	unit, cost := c.Policy.Unit, c.Cost
	var text string
	if cost.Low == cost.High {
		text = "Costs about " + qty(cost.High, unit)
	} else {
		text = "Costs " + fmtAmount(cost.Low) + "-" + qty(cost.High, unit)
	}
	if cost.Note != "" {
		return text + " (" + cost.Note + ")."
	}
	return text + "."
}

// Sentence naming the chat's spend so far or its limit, empty when neither applies
func (c *Charge) SpendSentence(already float64) string {
	unit, limit := c.Policy.Unit, c.Policy.Limit
	if already > 0 {
		capText := ""
		if limit > 0 {
			capText = fmt.Sprintf(" (limit %s)", fmtAmount(limit))
		}
		return fmt.Sprintf("This chat has used about %s on '%s'%s.", qty(already, unit), c.Server, capText)
	}
	if limit > 0 {
		return fmt.Sprintf("Spend limit for this chat on '%s': %s.", c.Server, qty(limit, unit))
	}
	return ""
}

// Cost and spend sentences for the approval prompt, empty ones dropped
func (c *Charge) Lines(already float64) []string {
	var out []string
	for _, s := range []string{c.CostSentence(), c.SpendSentence(already)} {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Tool result text for a call refused at the spend limit
func (c *Charge) Refusal(already float64) string {
	unit := c.Policy.Unit
	return fmt.Sprintf("MCP tool '%s' on server '%s' was NOT run: this chat has used about %s on '%s' and its spend limit is %s. "+
		"This call (about %s) needed an explicit approval and did not get one. Do not retry; tell the user the spend limit was reached.",
		c.Tool, c.Server, qty(already, unit), c.Server, qty(c.Policy.Limit, unit), qty(c.Cost.High, unit))
}

// Charge a call would add, nil for an undeclared or zero cost tool
func PendingCharge(ctx context.Context, policyKey ServerKey, serverName, toolName, sessionKey string) *Charge {
	policy := serverCostPolicy(policyKey)
	if policy == nil {
		return nil
	}
	cost, ok := policy.Costs[toolName]
	if !ok || cost.High <= 0 {
		return nil
	}
	session := runKey(ctx)
	if session == "" {
		session = sessionKey
	}
	return &Charge{key: tallyKey{Policy: policyKey, Session: session}, Server: serverName, Tool: toolName, Cost: cost, Policy: policy}
}

// Declared tool names the server does not offer, sorted, so registration can warn about the typo
func UnknownCostKeys(policy *CostPolicy, toolNames []string) []string {
	if policy == nil || len(toolNames) == 0 {
		return nil
	}
	offered := map[string]bool{}
	for _, n := range toolNames {
		offered[n] = true
	}
	var out []string
	for k := range policy.Costs {
		if !offered[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Cost policy registered for a connection key, nil when the server declares none
func serverCostPolicy(key ServerKey) *CostPolicy {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	return serverCostPolicies[key]
}
