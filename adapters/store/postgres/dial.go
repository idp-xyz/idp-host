package postgres

import (
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// How long a connection may go on without its peer answering before it counts as dead.
const (
	connectTimeout = 5 * time.Second  // a new connection (unless the URL has its own connect_timeout)
	keepAliveIdle  = 15 * time.Second // an idle connection is probed after this, then every 5 s, 3 times
	peerTimeout    = 30 * time.Second // sent data not acknowledged for this long ends the connection (TCP_USER_TIMEOUT)
)

// guardDial makes the connections of c notice a peer that vanished without a reset (its node lost, a network partition). pgx's
// defaults leave that to TCP: a query with no deadline then waits for the kernel to give up, 151 s measured on kind and up to
// some 15 minutes by the kernel's defaults. With this it fails after peerTimeout, the pool drops the connection and the next
// query gets a new one, to whoever is the primary by then.
func guardDial(c *pgconn.Config) {
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = connectTimeout
	}
	d := &net.Dialer{
		Timeout:         connectTimeout,
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: keepAliveIdle, Interval: 5 * time.Second, Count: 3},
		Control:         setPeerTimeout,
	}
	c.DialFunc = d.DialContext
}
