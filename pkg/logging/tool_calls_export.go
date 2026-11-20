package logging

import "context"

// Size rotated log file handler whose records pass the redacting formatter with the given format
func NewRedactingRotatingFileHandler(ctx context.Context, path string, maxBytes float64, backupCount int, format string) (Handler, error) {
	formatter, err := newRedactingFormatter(format, "")
	if err != nil {
		return nil, err
	}
	handler, err := newFileHandlerFn(ctx, path, LevelNotSet, maxBytes, backupCount, formatter)
	if err != nil {
		return nil, err
	}
	return handler, nil
}
