package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/installidentity"
)

// Invalid or conflicting hosted room operation, a ValueError
type HostedRoomError struct{ Msg string }

func (e *HostedRoomError) Error() string { return e.Msg }

// Python class name of the error
func (e *HostedRoomError) PyTypeName() string { return "HostedRoomError" }

func hostedRoomError(msg string) error { return &HostedRoomError{msg} }

// Bounded actor identifier of a hosted room field
func hostedActorID(value any, label string) (string, error) {
	return roomIdentifier(value, roomIdentifierSpec{label: label, maxChars: 128}, hostedRoomError)
}

// Stable server owned identity for hosted room authority, derived from the install identity
func LocalAuthorityGatewayID(ctx context.Context) (string, error) {
	installID, err := installidentity.GetInstallID(ctx, nil)
	if err != nil {
		return "", err
	}
	if installID == "" {
		return "", &HostedRoomError{"stable gateway install identity is unavailable"}
	}
	return hostedActorID("install:"+installID, "authority_gateway_id")
}
