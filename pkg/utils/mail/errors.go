package mailutil

import (
	"errors"
	"strconv"
	"strings"
)

// Error of an operation the server does not offer, such as STARTTLS or an authentication mechanism it lacks
var ErrNotSupported = errors.New("not supported by the server")

// Error matched by a login the server rejected for good: an SMTP AUTH exchange ended by a 5xx reply or an IMAP LOGIN answered NO, temporary failures excluded
var ErrAuthFailed = errors.New("authentication failed")

// Error of a login refused before any credentials were sent, the connection being unencrypted and plaintext logins not allowed for it
var ErrInsecure = errors.New("connection is not encrypted")

// Error matched by an IMAP command the server answered with NO
var ErrRefused = errors.New("refused by the server")

// Error of a mailbox the server opened for reading only
var ErrReadOnly = errors.New("mailbox is read-only")

// Error of a fetch that brought no content back, the message or the section asked for not existing
var ErrNotFound = errors.New("message not found")

// Marks errors of arguments a command cannot carry, which leave the connection usable
var errArgument = errors.New("invalid argument")

// TLS handshake with a mail server that failed for a reason other than a timeout, such as an untrusted certificate or a server that does not speak TLS
type TLSError struct {
	Host string // server name the certificate was checked against
	Err  error  // failure crypto/tls reported
}

// Message naming the server and the failure
func (e *TLSError) Error() string { return "TLS handshake with " + e.Host + ": " + e.Err.Error() }

// Failure crypto/tls reported
func (e *TLSError) Unwrap() error { return e.Err }

// Literal an IMAP server announced that is larger than the client keeps in memory, see SetMaxLiteral
type LiteralTooLargeError struct {
	Size    int64 // octets the server announced, the largest int64 for a count beyond it
	Limit   int64 // largest literal the client keeps
	Drained bool  // whether the literal was read past and dropped, leaving the connection usable; false when it was over 1 GiB and the connection was closed
}

// Message naming the announced size, the limit and whether the connection was closed
func (e *LiteralTooLargeError) Error() string {
	msg := "imap: server announced a literal of " + strconv.FormatInt(e.Size, 10) + " bytes, over the limit of " + strconv.FormatInt(e.Limit, 10)
	if !e.Drained {
		msg += "; connection closed"
	}
	return msg
}

// Reply with which an SMTP server ended a command unsuccessfully
type SMTPError struct {
	Command string // what was sent, such as MAIL FROM or AUTH PLAIN
	Code    int    // three digit reply code
	Message string // reply text, the lines of a multi line reply joined by line feeds
}

// Message with the command, the code and the reply text on one line
func (e *SMTPError) Error() string {
	return "smtp: " + e.Command + ": " + strconv.Itoa(e.Code) + " " + strings.ReplaceAll(e.Message, "\n", " ")
}

// Whether the target is ErrAuthFailed and a permanent 5xx reply ended an AUTH exchange
func (e *SMTPError) Is(target error) bool {
	return target == ErrAuthFailed && strings.HasPrefix(e.Command, "AUTH ") && e.Code >= 500
}

// Answer of NO or BAD an IMAP server gave a command
type IMAPError struct {
	Command string // command name, such as LOGIN or UID FETCH
	Status  string // NO or BAD
	Code    string // response code from inside the brackets, such as AUTHENTICATIONFAILED, empty when the server gave none
	Text    string // human readable text of the answer
}

// Message with the command, the status, the response code and the text
func (e *IMAPError) Error() string {
	var b strings.Builder
	b.WriteString("imap: ")
	b.WriteString(e.Command)
	b.WriteString(": ")
	b.WriteString(e.Status)
	if e.Code != "" {
		b.WriteString(" [")
		b.WriteString(e.Code)
		b.WriteByte(']')
	}
	if e.Text != "" {
		b.WriteByte(' ')
		b.WriteString(e.Text)
	}
	return b.String()
}

// Whether the target is ErrRefused and the answer was NO, or ErrAuthFailed and the answer was a NO to LOGIN that does not report a temporary outage
func (e *IMAPError) Is(target error) bool {
	switch target {
	case ErrRefused:
		return e.Status == "NO"
	case ErrAuthFailed:
		return e.Status == "NO" && e.Command == "LOGIN" && e.Code != "UNAVAILABLE"
	}
	return false
}

// Whether a failure left the protocol in step, so the connection can carry further commands
func keepsSession(err error) bool {
	var imapErr *IMAPError
	var smtpErr *SMTPError
	var tooLarge *LiteralTooLargeError
	return errors.As(err, &imapErr) || errors.As(err, &smtpErr) && smtpErr.Code != 421 ||
		errors.Is(err, ErrNotSupported) || errors.Is(err, ErrReadOnly) || errors.Is(err, ErrNotFound) || errors.Is(err, errArgument) ||
		errors.Is(err, ErrInsecure) || errors.As(err, &tooLarge) && tooLarge.Drained
}
