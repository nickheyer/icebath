package model

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/internal/httpclient"
)

var http2DropTexts = []string{"http2: server sent goaway", "http2: client connection lost", "stream error: stream id"}

func certVerifyFailure(err error) bool {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var roots x509.SystemRootsError
	var verify *tls.CertificateVerificationError
	return errors.As(err, &verify) || errors.As(err, &unknown) || errors.As(err, &invalid) ||
		errors.As(err, &hostname) || errors.As(err, &roots)
}

func tlsFailure(err error) bool {
	var he *httpclient.Error
	if errors.As(err, &he) && he.Phase == httpclient.PhaseTLS {
		return true
	}
	var alert tls.AlertError
	var record tls.RecordHeaderError
	if errors.As(err, &alert) || errors.As(err, &record) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && (op.Op == "remote error" || op.Op == "local error") &&
		op.Err != nil && strings.HasPrefix(op.Err.Error(), "tls: ")
}

func connectionDropped(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) || resetOrAborted(err) {
		return true
	}
	var he *httpclient.Error
	if !errors.As(err, &he) || he.Err == nil {
		return false
	}
	return errors.Is(he.Err, io.EOF) || containsAny(strings.ToLower(he.Err.Error()), http2DropTexts)
}

func transportFailure(err error) bool {
	var he *httpclient.Error
	var pe *httpclient.ProxyError
	var ne net.Error
	var errno syscall.Errno
	var sys *os.SyscallError
	var path *fs.PathError
	var lifetime *StreamLifetimeError
	return errors.As(err, &he) || errors.As(err, &lifetime) || errors.As(err, &pe) || errors.As(err, &ne) || errors.As(err, &errno) ||
		errors.As(err, &sys) || errors.As(err, &path) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, httpclient.ErrTimeout) ||
		certVerifyFailure(err) || tlsFailure(err)
}

func offlineFailure(err error) bool {
	var dns *net.DNSError
	return errors.As(err, &dns) || networkUnreachable(err)
}
