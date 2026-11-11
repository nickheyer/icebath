package tuigateway

import "sync"

// Process owned hosted room worker that stops without interrupting accepted turns
type hostedRoomService interface {
	Stop(timeout float64) bool
}

// The process owned hosted room service slot and its lock
var hostedRooms struct {
	sync.Mutex
	service hostedRoomService
}

// Stops the process owned hosted room worker, true when none runs or it stopped in time
func StopHostedRoomService(timeout float64) bool {
	hostedRooms.Lock()
	defer hostedRooms.Unlock()
	service := hostedRooms.service
	if service == nil {
		return true
	}
	stopped := service.Stop(timeout)
	if stopped && hostedRooms.service == service {
		hostedRooms.service = nil
	}
	return stopped
}
