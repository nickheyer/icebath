//go:build !darwin && !windows

package system

import (
	"os"
	"sync"

	"github.com/godbus/dbus/v5"
)

// org.gnome.SessionManager Inhibit flags
const (
	gnomeInhibitSuspendSession  = 4
	gnomeInhibitMarkSessionIdle = 8
)

type dbusCookie struct {
	conn   *dbus.Conn
	dest   string
	path   dbus.ObjectPath
	method string
	cookie uint32
}

type freedesktopHold struct {
	once    sync.Once
	cookies []dbusCookie
	fd      *os.File
}

func (h *freedesktopHold) Release() {
	h.once.Do(func() {
		for _, c := range h.cookies {
			c.conn.Object(c.dest, c.path).Call(c.method, 0, c.cookie)
		}
		if h.fd != nil {
			h.fd.Close()
		}
	})
}

func nameHasOwner(conn *dbus.Conn, name string) bool {
	var owned bool
	if err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, name).Store(&owned); err != nil {
		return false
	}
	return owned
}

// Inhibits idle sleep through every service present, GNOME session, freedesktop power or screensaver and logind idle
func platformInhibit(kind KeepAwakeType, reason string) Hold {
	hold := &freedesktopHold{}
	if session, err := dbus.SessionBus(); err == nil {
		flags := uint32(gnomeInhibitSuspendSession)
		if kind == PreventDisplaySleep {
			flags |= gnomeInhibitMarkSessionIdle
		}
		if nameHasOwner(session, "org.gnome.SessionManager") {
			var cookie uint32
			if session.Object("org.gnome.SessionManager", "/org/gnome/SessionManager").
				Call("org.gnome.SessionManager.Inhibit", 0, reason, uint32(0), reason, flags).Store(&cookie) == nil {
				hold.cookies = append(hold.cookies, dbusCookie{session, "org.gnome.SessionManager", "/org/gnome/SessionManager", "org.gnome.SessionManager.Uninhibit", cookie})
			}
		}
		dest, path, iface := "org.freedesktop.PowerManagement", dbus.ObjectPath("/org/freedesktop/PowerManagement/Inhibit"), "org.freedesktop.PowerManagement.Inhibit"
		if kind == PreventDisplaySleep {
			dest, path, iface = "org.freedesktop.ScreenSaver", "/org/freedesktop/ScreenSaver", "org.freedesktop.ScreenSaver"
		}
		if nameHasOwner(session, dest) {
			var cookie uint32
			if session.Object(dest, path).Call(iface+".Inhibit", 0, reason, reason).Store(&cookie) == nil {
				hold.cookies = append(hold.cookies, dbusCookie{session, dest, path, iface + ".UnInhibit", cookie})
			}
		}
	}
	if system, err := dbus.SystemBus(); err == nil {
		var fd dbus.UnixFD
		if system.Object("org.freedesktop.login1", "/org/freedesktop/login1").
			Call("org.freedesktop.login1.Manager.Inhibit", 0, "idle", reason, reason, "block").Store(&fd) == nil {
			hold.fd = os.NewFile(uintptr(fd), "logind-inhibit")
		}
	}
	return hold
}
