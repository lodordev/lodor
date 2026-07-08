//go:build android || lodorandroid

package clocksync

import (
	"errors"
	"time"
)

// setClock on Android: settimeofday is forbidden to apps (SELinux; no root) and never
// needed — Android devices keep a real, OS-managed clock, so Ensure's sane() gate means
// this is unreachable in practice. It exists so the package compiles under -tags android
// and answers honestly if ever reached.
var setClock = func(time.Time) error {
	return errors.New("android: settimeofday not permitted; clock is managed by the OS")
}
