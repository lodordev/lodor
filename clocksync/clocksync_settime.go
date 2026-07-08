//go:build !android && !lodorandroid

package clocksync

import (
	"syscall"
	"time"
)

// setClock sets the system wall clock. Linux-only by build; the device paks run as root.
var setClock = func(t time.Time) error {
	tv := syscall.NsecToTimeval(t.UnixNano())
	return syscall.Settimeofday(&tv)
}
