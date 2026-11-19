package tuigateway

import (
	"fmt"
	"log/slog"
	"os"
)

// A result or payload the gateway produced that does not fit its declared type
type ContractViolation struct{ Message string }

func (e *ContractViolation) Error() string { return e.Message }

// Misfits panic under test isolation and are logged otherwise
var PanicOnMisfit = os.Getenv("ICEBATH_TEST_ISOLATION") != ""

// Text of the method not found error both routes send
func UnknownMethodMessage(method string) string {
	return "unknown method: " + method + " — not available on this Icebath backend"
}

// Panics with a contract misfit under test isolation, logs it as an error otherwise
func ReportMisfit(kind, name string, misfit error) {
	if PanicOnMisfit {
		panic(&ContractViolation{fmt.Sprintf("%s %s violates its contract: %s", kind, name, misfit)})
	}
	slog.Default().With("logger", "tuigateway").Error(fmt.Sprintf("%s %s violates its wire contract: %s", kind, name, misfit))
}

// Payload of an event checked against its declared type, a misfit reported and the payload kept
func SealPayload(name string, payload map[string]any) map[string]any {
	if typed := Lookup(KindEvent, name); typed != nil {
		value := payload
		if value == nil {
			value = map[string]any{}
		}
		if err := typed.CheckResult(value); err != nil {
			ReportMisfit("payload of", name, err)
		}
	}
	return payload
}
