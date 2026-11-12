//go:build !windows

package flex

import "syscall"

// reuseAddr sets SO_REUSEADDR so discovery can share UDP 4992 with SmartSDR.
// The app ships for Windows only; this lets the package build and its tests
// run on other systems.
func reuseAddr(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
