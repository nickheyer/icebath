package tuigateway

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Process owned hosted room worker that stops without interrupting accepted turns
type HostedRoomService interface {
	StorePath() string
	Start() error
	Stop(timeout float64) bool
	RuntimeStatus() map[string]any
}

type hostedRoomServiceBox struct{ service HostedRoomService }

// The process owned hosted room service slot, its lock and the bound server constructor
var hostedRooms struct {
	sync.Mutex
	service       atomic.Pointer[hostedRoomServiceBox]
	newService    func(ctx context.Context, dbPath string) (HostedRoomService, error)
	defaultDBPath func(ctx context.Context) string
}

func currentHostedRoomService() HostedRoomService {
	if box := hostedRooms.service.Load(); box != nil {
		return box.service
	}
	return nil
}

func setHostedRoomService(service HostedRoomService) {
	if service == nil {
		hostedRooms.service.Store(nil)
		return
	}
	hostedRooms.service.Store(&hostedRoomServiceBox{service})
}

// Binds the initialized server's service constructor and room database default without starting a worker
func BindHostedRoomServer(newService func(ctx context.Context, dbPath string) (HostedRoomService, error), defaultDBPath func(ctx context.Context) string) {
	hostedRooms.Lock()
	defer hostedRooms.Unlock()
	hostedRooms.newService, hostedRooms.defaultDBPath = newService, defaultDBPath
}

// Starts one process owned hosted room service idempotently, nil without a bound server
func StartHostedRoomService() (HostedRoomService, error) {
	hostedRooms.Lock()
	defer hostedRooms.Unlock()
	if hostedRooms.newService == nil {
		return nil, nil
	}
	ctx := context.Background()
	dbPath := hostedRooms.defaultDBPath(ctx)
	service := currentHostedRoomService()
	if service != nil && service.StorePath() != dbPath {
		service.Stop(1.0)
		setHostedRoomService(nil)
		service = nil
	}
	if service == nil {
		created, err := hostedRooms.newService(ctx, dbPath)
		if err != nil {
			return nil, err
		}
		service = created
		setHostedRoomService(service)
	}
	if err := service.Start(); err != nil {
		return nil, err
	}
	return service, nil
}

// Stops the process owned hosted room worker, true when none runs or it stopped in time
func StopHostedRoomService(timeout float64) bool {
	hostedRooms.Lock()
	defer hostedRooms.Unlock()
	service := currentHostedRoomService()
	if service == nil {
		return true
	}
	stopped := service.Stop(timeout)
	if stopped && currentHostedRoomService() == service {
		setHostedRoomService(nil)
	}
	return stopped
}

// Active service when its lifecycle owner started it and it is running and not stopping, else nil
func GetHostedRoomService() HostedRoomService {
	service := currentHostedRoomService()
	if service == nil {
		return nil
	}
	status := service.RuntimeStatus()
	if pyrepr.Truthy(status["running"]) && !pyrepr.Truthy(status["stopping"]) {
		return service
	}
	return nil
}
