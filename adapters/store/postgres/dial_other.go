//go:build !linux

package postgres

import "syscall"

// TCP_USER_TIMEOUT is Linux's; elsewhere (a developer's machine) the keep-alive and the connect timeout remain.
func setPeerTimeout(_, _ string, _ syscall.RawConn) error { return nil }
