//go:build linux

package cloudhypervisor

import "golang.org/x/sys/unix"

func setConsoleSize(fileFD uintptr, rows, columns uint16) error {
	return unix.IoctlSetWinsize(int(fileFD), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: columns})
}
