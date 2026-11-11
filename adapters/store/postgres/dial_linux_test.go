package postgres

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The sockets the pool dials carry the peer timeout and the keep-alive; a connect_timeout from the URL is kept.
func TestGuardDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := pgconn.ParseConfig("postgres://u@127.0.0.1/db")
	if err != nil {
		t.Fatal(err)
	}
	guardDial(c)
	if c.ConnectTimeout != connectTimeout {
		t.Fatalf("connect timeout %v", c.ConnectTimeout)
	}
	conn, err := c.DialFunc(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var user, keep, idle int
	if err := raw.Control(func(fd uintptr) {
		user, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout)
		keep, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
		idle, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE)
	}); err != nil {
		t.Fatal(err)
	}
	if user != int(peerTimeout.Milliseconds()) || keep != 1 || idle != int(keepAliveIdle.Seconds()) {
		t.Fatalf("TCP_USER_TIMEOUT %d ms, SO_KEEPALIVE %d, TCP_KEEPIDLE %d s", user, keep, idle)
	}
	own, err := pgconn.ParseConfig("postgres://u@127.0.0.1/db?connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	guardDial(own)
	if own.ConnectTimeout != 2*time.Second {
		t.Fatalf("the URL's connect_timeout was replaced: %v", own.ConnectTimeout)
	}
}
