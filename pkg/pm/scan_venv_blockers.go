package pm

import (
	"fmt"
	"io"
)

// Matches a gateway command line, installed by the gateway status port, nil failing closed
var LooksLikeGatewayCommandLine func(cmdline string) bool

// Reports a pausable gateway command line, false while no matcher is installed
func isPausableGateway(cmdline string) bool {
	return LooksLikeGatewayCommandLine != nil && LooksLikeGatewayCommandLine(cmdline)
}

// Answers a historical Desktop venv preflight with one JSON document, never touching a process
func ScanVenvBlockersMain(argv []string, stdout io.Writer) int {
	doc := NewOrderedObject()
	code := 0
	if len(argv) > 0 && argv[0] == "--terminate-safe" {
		doc.Set("ok", false)
		doc.Set("error", "Legacy preview termination is retired; no processes were stopped.")
		code = 1
	} else {
		doc.Set("ok", true)
		doc.Set("blocked", false)
		doc.Set("processes", []any{})
		doc.Set("retired", true)
		doc.Set("message", "Legacy venv preflight is retired; PM stages fresh runtime generations.")
	}
	data, err := dumpsJSON(doc, false)
	if err != nil {
		return 1
	}
	fmt.Fprintln(stdout, string(data))
	return code
}
