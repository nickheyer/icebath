package gatewaycli

import (
	"context"
	"fmt"
)

// One SCM service whose state and process are queried when read
type WinService interface {
	Status() (string, error)
	PID() (int, error)
}

// The named SCM service, an error when the SCM cannot be listed or holds no service of that name
func WinServiceGet(name string) (WinService, error) {
	if scmEnumerate == nil {
		return nil, &scmQueryError{Class: "NoSuchProcess", Err: fmt.Errorf("service %q not found", name)}
	}
	services, err := scmEnumerate()
	if err != nil {
		return nil, err
	}
	for _, service := range services {
		if listed, err := service.Name(); err == nil && listed == name {
			return service, nil
		}
	}
	return nil, &scmQueryError{Class: "NoSuchProcess", Err: fmt.Errorf("service %q not found", name)}
}

// Gateways of the given profile processes that descend from a running Icebath owned service
func FindWindowsGatewayServicesFor(ctx context.Context, profileProcesses []ProfileGatewayProcess) ([]WindowsGatewayService, error) {
	return findWindowsGatewayServices(ctx, profileProcesses, true)
}
