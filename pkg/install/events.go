package install

type LogStream string

const (
	Stdout LogStream = "stdout"
	Stderr LogStream = "stderr"
)

// One output line of an optional stage on one stream
type Event struct {
	Stage  *string
	Line   string
	Stream LogStream
}

// Outcome of one stage with its skip flag and failure reason
type StageResult struct {
	Stage   string
	OK      bool
	Skipped bool
	Reason  *string
}

func strPtr(s string) *string { return &s }

// Log line event of an optional stage on one stream
func LogEvent(stage, line string, stream LogStream) Event {
	e := Event{Line: line, Stream: stream}
	if stage != "" {
		e.Stage = strPtr(stage)
	}
	return e
}
