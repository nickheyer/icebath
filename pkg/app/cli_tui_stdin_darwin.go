//go:build darwin

package app

import "golang.org/x/sys/unix"

// Switches terminal input to a select poller when kqueue cannot watch standard input
func tuiStdinSelectFallback() {
	if kqueueWatchesFD(0) != nil {
		stdinSelectLoop.Store(true)
	}
}

// Registers then removes a read watch on the descriptor in a fresh kqueue, the error when either step fails
func kqueueWatchesFD(fd int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	var change [1]unix.Kevent_t
	unix.SetKevent(&change[0], fd, unix.EVFILT_READ, unix.EV_ADD)
	if _, err := unix.Kevent(kq, change[:], nil, nil); err != nil {
		return err
	}
	unix.SetKevent(&change[0], fd, unix.EVFILT_READ, unix.EV_DELETE)
	_, err = unix.Kevent(kq, change[:], nil, nil)
	return err
}
