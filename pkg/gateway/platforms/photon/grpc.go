package photon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// gRPC limits and retry ladder of the Advanced iMessage transport
const (
	maxMessageBytes   = 100 * 1024 * 1024
	keepaliveTime     = 30 * time.Second
	keepaliveTimeout  = 20 * time.Second
	retryAttempts     = 4
	retryInitialDelay = 200 * time.Millisecond
	retryMaxDelay     = 5 * time.Second
	imessageService   = "/photon.imessage.v1."
)

// Methods that change server state and carry an idempotency key
var mutatingMethods = map[string]bool{
	"AttachmentService/UploadAttachment": true, "ChatService/CreateChat": true, "ChatService/MarkChatRead": true,
	"ChatService/ShareContactInfo": true, "ChatService/SetTyping": true, "MessageService/SendAttachmentMessage": true,
	"MessageService/SendTextMessage": true, "MessageService/SetReaction": true, "PollService/CreatePoll": true,
}

// Request or response bytes passed through gRPC untouched, the messages being encoded by hand
type rawMsg struct{ b []byte }

type passCodec struct{}

func (passCodec) Name() string { return "proto" }

func (passCodec) Marshal(v any) ([]byte, error) {
	m, ok := v.(*rawMsg)
	if !ok {
		return nil, fmt.Errorf("photon codec cannot marshal %T", v)
	}
	return m.b, nil
}

func (passCodec) Unmarshal(data []byte, v any) error {
	m, ok := v.(*rawMsg)
	if !ok {
		return fmt.Errorf("photon codec cannot unmarshal into %T", v)
	}
	m.b = append([]byte(nil), data...)
	return nil
}

// Opens a TLS gRPC channel with the keepalive and message limits the SDK channels use
func dialChannel(address string) (*grpc.ClientConn, error) {
	return grpc.NewClient(address,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: keepaliveTime, Timeout: keepaliveTimeout, PermitWithoutStream: true}),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(passCodec{}), grpc.MaxCallRecvMsgSize(maxMessageBytes),
			grpc.MaxCallSendMsgSize(maxMessageBytes)),
		grpc.WithUserAgent("icebath-photon"))
}

// Failure of an iMessage RPC with the server error code, retryability and context metadata
type rpcError struct {
	Code      codes.Code
	Detail    string
	ErrorCode string
	Retryable bool
	Context   map[string]string
}

func (e *rpcError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return strings.ToLower(e.Code.String())
}

func (e *rpcError) notFound() bool { return e.Code == codes.NotFound }

// Invalid argument and failed precondition, the codes a rejected resume cursor surfaces as
func (e *rpcError) validation() bool {
	return e.Code == codes.InvalidArgument || e.Code == codes.FailedPrecondition
}

// Status and trailer metadata of a failed call folded into an rpcError
func toRPCError(err error, trailer metadata.MD) error {
	if err == nil {
		return nil
	}
	var re *rpcError
	if errors.As(err, &re) {
		return re
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	out := &rpcError{Code: st.Code(), Detail: st.Message(), ErrorCode: "internalError", Context: map[string]string{}}
	if v := trailer.Get("error-code"); len(v) > 0 && v[0] != "" {
		out.ErrorCode = v[0]
	}
	if v := trailer.Get("x-retryable"); len(v) > 0 {
		out.Retryable = v[0] == "true"
	}
	for key, values := range trailer {
		if suffix, found := strings.CutPrefix(key, "error-context-"); found && suffix != "" && len(values) > 0 {
			out.Context[suffix] = values[0]
		}
	}
	return out
}

func errMissingField(field string) error {
	return &rpcError{Code: codes.Internal, Detail: "expected " + field + " in response", ErrorCode: "internalError"}
}

// The rpcError inside err, nil for other failures
func asRPCError(err error) *rpcError {
	var re *rpcError
	if errors.As(err, &re) {
		return re
	}
	return nil
}

// Outgoing context carrying the bearer token and, for writes, a fresh idempotency key
func callContext(ctx context.Context, token, method string) context.Context {
	md := metadata.Pairs("authorization", "Bearer "+token)
	if mutatingMethods[method] {
		md.Set("x-idempotency-key", uuid.NewString())
	}
	return metadata.NewOutgoingContext(ctx, md)
}

// Unary call retried with jittered backoff while the server marks the failure retryable
func unaryCall(ctx context.Context, conn *grpc.ClientConn, token, method string, req []byte) ([]byte, error) {
	var lastErr error
	for attempt := range retryAttempts {
		var trailer metadata.MD
		resp := &rawMsg{}
		err := conn.Invoke(callContext(ctx, token, method), imessageService+method, &rawMsg{b: req}, resp, grpc.Trailer(&trailer))
		if err == nil {
			return resp.b, nil
		}
		lastErr = toRPCError(err, trailer)
		re := asRPCError(lastErr)
		if re == nil || !re.Retryable || attempt == retryAttempts-1 {
			return nil, lastErr
		}
		delay := min(retryInitialDelay<<attempt, retryMaxDelay)
		timer := time.NewTimer(time.Duration(rand.Int64N(int64(delay) + 1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, lastErr
		case <-timer.C:
		}
	}
	return nil, lastErr
}

// Server streaming call whose request is sent and half closed at once
type serverStream struct {
	cs     grpc.ClientStream
	cancel context.CancelFunc
}

func openServerStream(ctx context.Context, conn *grpc.ClientConn, token, method string, req []byte) (*serverStream, error) {
	sctx, cancel := context.WithCancel(ctx)
	cs, err := conn.NewStream(callContext(sctx, token, method), &grpc.StreamDesc{ServerStreams: true}, imessageService+method)
	if err != nil {
		cancel()
		return nil, toRPCError(err, nil)
	}
	if err := cs.SendMsg(&rawMsg{b: req}); err != nil && !errors.Is(err, io.EOF) {
		cancel()
		return nil, toRPCError(err, cs.Trailer())
	}
	if err := cs.CloseSend(); err != nil {
		cancel()
		return nil, toRPCError(err, cs.Trailer())
	}
	return &serverStream{cs: cs, cancel: cancel}, nil
}

// Next frame bytes, io.EOF once the server half closes cleanly
func (s *serverStream) recv() ([]byte, error) {
	m := &rawMsg{}
	if err := s.cs.RecvMsg(m); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, toRPCError(err, s.cs.Trailer())
	}
	return m.b, nil
}

func (s *serverStream) close() { s.cancel() }
