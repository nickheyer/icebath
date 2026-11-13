//go:build !darwin && !windows

package system

import (
	"os"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	login1Service  = "org.freedesktop.login1"
	login1Path     = dbus.ObjectPath("/org/freedesktop/login1")
	login1Manager  = "org.freedesktop.login1.Manager"
	login1Session  = "org.freedesktop.login1.Session"
	upowerService  = "org.freedesktop.UPower"
	upowerPath     = dbus.ObjectPath("/org/freedesktop/UPower")
	dbusProperties = "org.freedesktop.DBus.Properties"
)

// FreedesktopPower watches logind sleep and session lock and UPower for the battery state
type FreedesktopPower struct {
	system *dbus.Conn
	sess   dbus.ObjectPath
}

// Power monitor over the system bus
func NewPowerMonitor() *FreedesktopPower { return &FreedesktopPower{} }

// Subscribes to PrepareForSleep, the session LockedHint and UPower OnBattery
func (p *FreedesktopPower) Start(ev PowerEvents) error {
	conn, err := dbus.SystemBus()
	if err != nil {
		return err
	}
	p.system = conn
	login := conn.Object(login1Service, login1Path)
	var session dbus.ObjectPath
	if id := os.Getenv("XDG_SESSION_ID"); id != "" {
		login.Call(login1Manager+".GetSession", 0, id).Store(&session)
	}
	if session == "" {
		login.Call(login1Manager+".GetSessionByPID", 0, uint32(os.Getpid())).Store(&session)
	}
	p.sess = session
	if err := conn.AddMatchSignal(dbus.WithMatchObjectPath(login1Path), dbus.WithMatchInterface(login1Manager), dbus.WithMatchMember("PrepareForSleep")); err != nil {
		return err
	}
	conn.AddMatchSignal(dbus.WithMatchObjectPath(upowerPath), dbus.WithMatchInterface(dbusProperties), dbus.WithMatchMember("PropertiesChanged"))
	if session != "" {
		conn.AddMatchSignal(dbus.WithMatchObjectPath(session), dbus.WithMatchInterface(dbusProperties), dbus.WithMatchMember("PropertiesChanged"))
	}
	locked := p.sessionLocked()
	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	go func() {
		for signal := range signals {
			switch {
			case signal.Path == login1Path && signal.Name == login1Manager+".PrepareForSleep" && len(signal.Body) == 1:
				if sleeping, ok := signal.Body[0].(bool); ok && sleeping {
					ev.Suspend()
				} else if ok {
					ev.Resume()
				}
			case signal.Name == dbusProperties+".PropertiesChanged" && len(signal.Body) >= 2:
				iface, _ := signal.Body[0].(string)
				changed, _ := signal.Body[1].(map[string]dbus.Variant)
				if signal.Path == upowerPath && iface == upowerService {
					if v, ok := changed["OnBattery"]; ok {
						if battery, ok := v.Value().(bool); ok && battery {
							ev.OnBattery()
						} else if ok {
							ev.OnAC()
						}
					}
				}
				if signal.Path == p.sess && iface == login1Session {
					if v, ok := changed["LockedHint"]; ok {
						now, _ := v.Value().(bool)
						if locked && !now {
							ev.UnlockScreen()
						}
						locked = now
					}
				}
			}
		}
	}()
	return nil
}

func (p *FreedesktopPower) sessionLocked() bool {
	if p.sess == "" {
		return false
	}
	v, err := p.system.Object(login1Service, p.sess).GetProperty(login1Session + ".LockedHint")
	if err != nil {
		return false
	}
	locked, _ := v.Value().(bool)
	return locked
}

// UPower OnBattery, false when UPower is absent
func (p *FreedesktopPower) IsOnBatteryPower() bool {
	if p.system == nil {
		return false
	}
	v, err := p.system.Object(upowerService, upowerPath).GetProperty(upowerService + ".OnBattery")
	if err != nil {
		return false
	}
	battery, _ := v.Value().(bool)
	return battery
}

// Idle seconds from the compositor idle monitor, the screensaver service or the logind idle hint
func (p *FreedesktopPower) SystemIdleTime() int {
	if session, err := dbus.SessionBus(); err == nil {
		var ms uint64
		if session.Object("org.gnome.Mutter.IdleMonitor", "/org/gnome/Mutter/IdleMonitor/Core").
			Call("org.gnome.Mutter.IdleMonitor.GetIdletime", 0).Store(&ms) == nil {
			return int(ms / 1000)
		}
		var seconds uint32
		if session.Object("org.freedesktop.ScreenSaver", "/org/freedesktop/ScreenSaver").
			Call("org.freedesktop.ScreenSaver.GetSessionIdleTime", 0).Store(&seconds) == nil {
			return int(seconds)
		}
	}
	if p.system == nil || p.sess == "" {
		return 0
	}
	session := p.system.Object(login1Service, p.sess)
	idle, err := session.GetProperty(login1Session + ".IdleHint")
	if err != nil {
		return 0
	}
	if on, _ := idle.Value().(bool); !on {
		return 0
	}
	since, err := session.GetProperty(login1Session + ".IdleSinceHint")
	if err != nil {
		return 0
	}
	us, _ := since.Value().(uint64)
	if us == 0 {
		return 0
	}
	return int(time.Since(time.UnixMicro(int64(us))).Seconds())
}
