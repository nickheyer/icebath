package tuihost

import (
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
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
	return slog.Default().With("logger", "tuigateway")
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

// Panics with a typed contract misfit under test isolation, logs it as an error otherwise
func ReportMisfit(kind, name string, misfit error) {
	if Strict {
		panic(&ContractViolation{fmt.Sprintf("%s %s violates its contract: %s", kind, name, misfit)})
	}
	contractsLog().Error(fmt.Sprintf("%s %s violates its wire contract: %s", kind, name, misfit))
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
				return nil, fmt.Sprintf("invalid params for %s: %s: %s — the client and the shared gateway run different Icebath builds; run `icebath gateway restart` to move the gateway to the client's build",
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

// Payload of an event checked against its typed contract or its pydantic one, a misfit reported and the payload kept
func SealPayload(name string, payload map[string]any) map[string]any {
	if typed := tuigateway.Lookup(tuigateway.KindEvent, name); typed != nil {
		value := payload
		if value == nil {
			value = map[string]any{}
		}
		if err := typed.CheckResult(value); err != nil {
			ReportMisfit("payload of", name, err)
		}
		return payload
	}
	contractsMu.RLock()
	contract := Events[name]
	contractsMu.RUnlock()
	if contract == nil {
		return payload
	}
	if contract.Payload == nil {
		if len(payload) > 0 {
			report("payload of", name, noPayloadError(payload))
		}
		return payload
	}
	value := payload
	if value == nil {
		value = map[string]any{}
	}
	if exc := contract.Payload.ModelValidate(value); exc != nil {
		report("payload of", name, exc)
	}
	return payload
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
