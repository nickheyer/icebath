package logging

import (
	"bufio"
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
)

// Timestamp a line already opens with, the shape the log readers filter on
var stampPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d{3}(?:\s|$)`)

// Line with a leading timestamp of now unless it already opens with one, its line ending replaced by one newline
func StampLine(line string) string {
	rendered := strings.TrimRight(line, "\r\n")
	if stampPrefix.MatchString(rendered) {
		return rendered + "\n"
	}
	return Timestamp(time.Now()) + " " + rendered + "\n"
}

// Copies each line of src to dst redacted and stamped, invalid UTF-8 replaced, newlines written as text files end
func (s *System) CopyStamped(dst io.Writer, src io.Reader) error {
	r := bufio.NewReader(src)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			text := StampLine(s.redact.Redact(context.Background(), decodeReplacing(line)))
			if _, werr := io.WriteString(dst, strings.ReplaceAll(text, "\n", lineEnding)); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
