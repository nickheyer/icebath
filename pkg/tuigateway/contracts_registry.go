package tuigateway

import (
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// One problem a wire model found in a value, the fields of a pydantic error entry
type ValidationIssue = pydantic.ErrorDetails

// Every problem one validation found, titled with the model that rejected the value
type ValidationError = pydantic.ValidationError

// Contract with a params model and a result model, a client method or a server request
type paramsContract interface {
	contractName() string
	paramsModel() *pydantic.Model
	resultModel() *pydantic.Model
}

// Client to server method with its params and result models
type MethodContract struct {
	Name   string
	Params *pydantic.Model
	Result *pydantic.Model
	Doc    string
}

func (c *MethodContract) contractName() string         { return c.Name }
func (c *MethodContract) paramsModel() *pydantic.Model { return c.Params }
func (c *MethodContract) resultModel() *pydantic.Model { return c.Result }

// Question the backend asks the client with its params and result models
type ServerRequestContract struct {
	Name   string
	Params *pydantic.Model
	Result *pydantic.Model
	Doc    string
}

func (c *ServerRequestContract) contractName() string         { return c.Name }
func (c *ServerRequestContract) paramsModel() *pydantic.Model { return c.Params }
func (c *ServerRequestContract) resultModel() *pydantic.Model { return c.Result }

// Notification with its payload model, nil when the frame carries no payload
type EventContract struct {
	Name    string
	Payload *pydantic.Model
	Doc     string
}

// The contract catalog, filled by the topic declarations at init and read by runtime validation
var (
	contractsMu      sync.RWMutex
	Methods          = map[string]*MethodContract{}
	CanonicalMethods = map[string]*MethodContract{}
	ServerRequests   = map[string]*ServerRequestContract{}
	Events           = map[string]*EventContract{}
	// Contract violations panic under test isolation and are logged otherwise
	Strict = os.Getenv("ICEBATH_TEST_ISOLATION") != ""
)

func contractsLog() *slog.Logger {
	return slog.Default().With("logger", "tui_gateway.contracts.registry")
}

// Text of the method not found error both routes send
func UnknownMethodMessage(method string) string {
	return "unknown method: " + method + " — not available on this Icebath backend"
}

// Adds an entry under its name, panicking when the table already declares that name
func declare[T any](table map[string]*T, name string, entry *T) {
	contractsMu.Lock()
	defer contractsMu.Unlock()
	if _, dup := table[name]; dup {
		panic("contract declared twice: " + name)
	}
	table[name] = entry
}

// Declares a client method contract
func Method(name string, params, result *pydantic.Model, doc string) *MethodContract {
	entry := &MethodContract{name, params, result, doc}
	declare(Methods, name, entry)
	return entry
}

// Declares a method only the canonical session authority route serves
func CanonicalMethod(name string, params, result *pydantic.Model, doc string) *MethodContract {
	entry := &MethodContract{name, params, result, doc}
	declare(CanonicalMethods, name, entry)
	return entry
}

// Declares a server to client request contract
func ServerRequestOf(name string, params, result *pydantic.Model, doc string) *ServerRequestContract {
	entry := &ServerRequestContract{name, params, result, doc}
	declare(ServerRequests, name, entry)
	return entry
}

// Declares an event contract, a nil payload model for a frame that carries none
func Event(name string, payload *pydantic.Model, doc string) *EventContract {
	entry := &EventContract{name, payload, doc}
	declare(Events, name, entry)
	return entry
}

// Result or payload the gateway produced that does not match its declared contract
type ContractViolation struct{ Message string }

func (e *ContractViolation) Error() string { return e.Message }

// Panics with the violation under test isolation, logs it as an error otherwise
func report(kind, name string, exc *ValidationError) {
	if Strict {
		panic(&ContractViolation{fmt.Sprintf("%s %s violates its contract: %s", kind, pyrepr.Repr(name), exc)})
	}
	contractsLog().Error(fmt.Sprintf("%s %s violates its wire contract: %s", kind, pyrepr.Repr(name), exc))
}

// The params unchanged, or the 4000 message naming the first key the contract does not declare
func ValidateParams(contract paramsContract, params any) (any, string) {
	if exc := contract.paramsModel().ModelValidate(params); exc != nil {
		for _, issue := range exc.Errors() {
			if issue.Type == "extra_forbidden" {
				loc := issue.LocText()
				if loc == "" {
					loc = "params"
				}
				return nil, fmt.Sprintf("invalid params for %s: %s: %s — the client and the Icebath backend are out of sync (different versions); run `icebath update` and restart both",
					contract.contractName(), loc, issue.Msg)
			}
		}
	}
	return params, ""
}

// Key paths the canonical dispatcher refuses, unknown and missing keys, or every non missing problem of a shared method
func CanonicalParamProblems(contract *MethodContract, params any) []string {
	exc := contract.Params.ModelValidate(params)
	if exc == nil {
		return []string{}
	}
	contractsMu.RLock()
	_, shared := Methods[contract.Name]
	contractsMu.RUnlock()
	problems := []string{}
	for _, issue := range exc.Errors() {
		refused := issue.Type == "extra_forbidden" || issue.Type == "missing"
		if shared {
			refused = issue.Type != "missing"
		}
		if refused {
			loc := issue.LocText()
			if loc == "" {
				loc = "params"
			}
			problems = append(problems, loc)
		}
	}
	return problems
}

// Reports params a handler answered that the contract calls invalid
func CheckParamsAccepted(contract paramsContract, params any) {
	if exc := contract.paramsModel().ModelValidate(params); exc != nil {
		report("params accepted by", contract.contractName(), exc)
	}
}

// Reports a result that does not match the result model of its contract
func CheckResult(contract paramsContract, result any) {
	if exc := contract.resultModel().ModelValidate(result); exc != nil {
		report("result of", contract.contractName(), exc)
	}
}

// Reports an event payload that violates its declared contract, an undeclared event passing unchecked
func CheckPayload(name string, payload map[string]any) {
	contractsMu.RLock()
	contract := Events[name]
	contractsMu.RUnlock()
	if contract == nil {
		return
	}
	if contract.Payload == nil {
		if len(payload) > 0 {
			report("payload of", name, noPayloadError(payload))
		}
		return
	}
	value := payload
	if value == nil {
		value = map[string]any{}
	}
	if exc := contract.Payload.ModelValidate(value); exc != nil {
		report("payload of", name, exc)
	}
}

// Python list text of sorted names
func sortedNamesRepr(names []string) string {
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = pyrepr.Repr(n)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Names of a that b lacks
func namesMissing(a, b []string) []string {
	out := []string{}
	for _, n := range a {
		if !slices.Contains(b, n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// Checks that every method, emitted event and sent server request has a contract and no contract is orphaned
func AssertComplete(methods, emittedEvents, serverRequests []string) error {
	contractsMu.RLock()
	declaredMethods, declaredEvents, declaredRequests := mapNames(Methods), mapNames(Events), mapNames(ServerRequests)
	contractsMu.RUnlock()
	problems := []string{}
	add := func(label string, names []string) {
		if len(names) > 0 {
			problems = append(problems, label+": "+sortedNamesRepr(names))
		}
	}
	add("methods without a contract", namesMissing(methods, declaredMethods))
	add("method contracts with no handler", namesMissing(declaredMethods, methods))
	add("events without a contract", namesMissing(emittedEvents, declaredEvents))
	add("event contracts nothing emits", namesMissing(declaredEvents, emittedEvents))
	add("server requests without a contract", namesMissing(serverRequests, declaredRequests))
	add("server request contracts nothing sends", namesMissing(declaredRequests, serverRequests))
	if len(problems) > 0 {
		return &ContractViolation{"tui_gateway/contracts is incomplete:\n  " + strings.Join(problems, "\n  ")}
	}
	return nil
}

func mapNames[T any](table map[string]*T) []string {
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	return names
}

// Payload model with no fields, every key of a payload it is given an extra key
var emptyPayload = pydantic.NewModel("_Empty", ClosedConfig)

// The extra key error a payload earns on an event declared to carry none
func noPayloadError(payload map[string]any) *ValidationError {
	exc := emptyPayload.ModelValidate(payload)
	if exc == nil {
		panic("unreachable")
	}
	return exc
}
