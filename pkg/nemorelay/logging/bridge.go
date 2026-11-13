package logging

import (
	"errors"
	"strconv"
	"sync"
)

// Diagnostic code of one logging observation
type LoggingCode int

const (
	CodeLoggingInitialized LoggingCode = iota
	CodeLoggingShutdownCompleted
	CodeLoggingInitializeFailed
	CodeLoggingShutdownFailed
	CodeLoggingProjectionUnavailable
	CodeSinkInitialized
	CodeSinkInitializeFailed
	CodeSinkRecordDropped
	CodeSinkFlushDropped
	CodeSinkFlushFailed
	CodeSinkWriteFailed
	CodeSinkShutdownFailed
)

// Failure class attached to a logging failure observation
type FailureClass int

const (
	FailureClassNone FailureClass = iota
	FailureClassUnknown
	FailureClassIO
	FailureClassResourceExhausted
)

// Records logging evidence for one registered diagnostics component, never projecting it back into the log
type EvidenceRecorder interface {
	Record(code LoggingCode, class FailureClass, severity Level, message string)
}

// Diagnostics service context logging records into and projects observations from
type DiagnosticsContext interface {
	SourceInstanceID() string
	Available() bool
	RegisterRuntimeComponent(name string) (EvidenceRecorder, error)
	RegisterSinkComponent(name string) (EvidenceRecorder, error)
	SubscribeProjection(project func(severity Level, code string, summaryJSON []byte) error) (unsubscribe func(), err error)
}

// Errors a projection returns for an observation it could not log
var (
	ErrProjectionPayloadTooLarge     = errors.New("diagnostic projection payload is too large")
	ErrProjectionSerializationFailed = errors.New("diagnostic projection serialization failed")
)

var (
	diagnosticsSourceMu sync.Mutex
	diagnosticsSource   func() DiagnosticsContext
)

// Installs the lookup of the process selected diagnostics context, nil meaning none selected
func SetDiagnosticsSource(source func() DiagnosticsContext) {
	diagnosticsSourceMu.Lock()
	diagnosticsSource = source
	diagnosticsSourceMu.Unlock()
}

func selectedDiagnostics() DiagnosticsContext {
	diagnosticsSourceMu.Lock()
	source := diagnosticsSource
	diagnosticsSourceMu.Unlock()
	if source == nil {
		return nil
	}
	return source()
}

// Sink evidence slot filled when diagnostics attaches after logging started
type SinkRecorder interface {
	recorder() EvidenceRecorder
}

type sinkSlot struct {
	mu  sync.Mutex
	rec EvidenceRecorder
}

func (s *sinkSlot) recorder() EvidenceRecorder {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec
}

func (s *sinkSlot) setOnce(rec EvidenceRecorder) {
	s.mu.Lock()
	if s.rec == nil {
		s.rec = rec
	}
	s.mu.Unlock()
}

func recordInto(rec EvidenceRecorder, code LoggingCode, class FailureClass, severity Level, message string) {
	if rec != nil {
		rec.Record(code, class, severity, message)
	}
}

func sinkRecorder(s SinkRecorder) EvidenceRecorder {
	if s == nil {
		return nil
	}
	return s.recorder()
}

// Records a sink delivery failure with the message its classification selects
func recordSinkRuntimeError(s SinkRecorder, err error) {
	rec := sinkRecorder(s)
	if rec == nil {
		return
	}
	var se *sinkError
	if errors.As(err, &se) && se.kind == "flush buffer" {
		rec.Record(CodeSinkFlushFailed, FailureClassUnknown, LevelError, "Operational log sink flush failed")
		return
	}
	rec.Record(CodeSinkWriteFailed, FailureClassUnknown, LevelError, "Operational log sink failed to deliver a record")
}

// Records a record or flush request dropped by a full sink queue
func recordSinkDropped(s SinkRecorder, flush bool) {
	rec := sinkRecorder(s)
	if rec == nil {
		return
	}
	if flush {
		rec.Record(CodeSinkFlushDropped, FailureClassResourceExhausted, LevelError, "Operational log flush request dropped because the sink queue is full")
		return
	}
	rec.Record(CodeSinkRecordDropped, FailureClassResourceExhausted, LevelError, "Operational log record dropped because the sink queue is full")
}

// Evidence recorders of the logging runtime and each sink
type loggingDiagnostics struct {
	runtime       EvidenceRecorder
	stderrEnabled bool
	stderr        *sinkSlot
	files         []*sinkSlot
}

func newLoggingDiagnostics(context DiagnosticsContext, config LoggingConfig) *loggingDiagnostics {
	d := &loggingDiagnostics{stderrEnabled: config.StderrEnabled}
	register := func(key string) *sinkSlot {
		slot := &sinkSlot{}
		if context != nil && context.Available() {
			if rec, err := context.RegisterSinkComponent(key); err == nil {
				slot.setOnce(rec)
			}
		}
		return slot
	}
	if context != nil {
		if rec, err := context.RegisterRuntimeComponent("logging"); err == nil {
			d.runtime = rec
		}
	}
	if config.StderrEnabled {
		d.stderr = register("logging.stderr")
	}
	for index := range config.Sinks {
		d.files = append(d.files, register(fileComponentName(index)))
	}
	return d
}

func (d *loggingDiagnostics) attach(context DiagnosticsContext) {
	if rec, err := context.RegisterRuntimeComponent("logging"); err == nil {
		d.runtime = rec
	} else {
		d.runtime = nil
	}
	attach := func(slot *sinkSlot, key string) {
		if rec, err := context.RegisterSinkComponent(key); err == nil {
			slot.setOnce(rec)
		}
	}
	if d.stderr != nil {
		attach(d.stderr, "logging.stderr")
	}
	for index, file := range d.files {
		if file != nil {
			attach(file, fileComponentName(index))
		}
	}
}

func (d *loggingDiagnostics) file(index int) *sinkSlot {
	if index < len(d.files) {
		return d.files[index]
	}
	return nil
}

// Sink slot of a logger sink position, the stderr sink first when enabled
func (d *loggingDiagnostics) sink(index int) *sinkSlot {
	if d.stderrEnabled {
		if index == 0 {
			return d.stderr
		}
		return d.file(index - 1)
	}
	return d.file(index)
}

func (d *loggingDiagnostics) initialized() {
	recordInto(d.runtime, CodeLoggingInitialized, FailureClassNone, LevelInfo, "Operational logging initialized")
}

func (d *loggingDiagnostics) initializationFailed() {
	recordInto(d.runtime, CodeLoggingInitializeFailed, FailureClassUnknown, LevelError, "Operational logging initialization failed")
}

func (d *loggingDiagnostics) shutdown(failed bool) {
	if failed {
		recordInto(d.runtime, CodeLoggingShutdownFailed, FailureClassUnknown, LevelError, "Operational logging shutdown encountered sink failures")
		return
	}
	recordInto(d.runtime, CodeLoggingShutdownCompleted, FailureClassNone, LevelInfo, "Operational logging shutdown completed")
}

func (d *loggingDiagnostics) projectionUnavailable() {
	recordInto(d.runtime, CodeLoggingProjectionUnavailable, FailureClassUnknown, LevelWarn, "Operational logging is available but the diagnostic projection could not be attached")
}

func slotInitialized(s *sinkSlot) {
	recordInto(s.recorder(), CodeSinkInitialized, FailureClassNone, LevelInfo, "Logging sink initialized")
}

func slotInitializationFailed(s *sinkSlot) {
	recordInto(s.recorder(), CodeSinkInitializeFailed, FailureClassUnknown, LevelError, "Logging sink initialization failed")
}

func slotOpenFailed(s *sinkSlot, err error) {
	recordInto(s.recorder(), CodeSinkInitializeFailed, FailureClassIO, LevelError, fileOpenFailureReason(err))
}

func slotShutdownFailed(s *sinkSlot) {
	recordInto(s.recorder(), CodeSinkShutdownFailed, FailureClassUnknown, LevelError, "Operational log sink final drain or flush failed")
}

func fileComponentName(index int) string {
	return "logging.file." + strconv.Itoa(index)
}
