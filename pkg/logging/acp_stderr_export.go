package logging

import (
	"context"
	"time"
)

// Logs msg at ERROR with the error text below it, the error carried on the record for filters
func (l *Logger) Exception(msg string, err error) {
	if !l.IsEnabledFor(LevelError) {
		return
	}
	ctx := context.Background()
	rec := makeRecord(ctx, l.name, LevelError, msg+"\n"+err.Error(), time.Now(), 0)
	rec.ExcInfo = err
	l.handle(ctx, rec)
}

// Stream handler whose filters and formatter a caller sets up
type StreamHandler interface {
	Handler
	SetLevel(level int)
	AddFilter(f Filter)
	Formatter() *Formatter
}

// Handler writing records to stderr through a redacting formatter of the given format
func NewRedactingStderrHandler(format, datefmt string) (StreamHandler, error) {
	formatter, err := newRedactingFormatter(format, datefmt)
	if err != nil {
		return nil, err
	}
	h := newStreamHandler(safeStderr())
	h.SetFormatter(formatter)
	return h, nil
}
