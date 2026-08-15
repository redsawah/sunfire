package transport

import (
	"net"
	"time"
)

type tcpTransport struct{}

func (tcpTransport) Name() string { return "tcp" }

func (tcpTransport) Dial(addr, _ string, timeout time.Duration) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	return c, nil
}

func (tcpTransport) Listen(addr, _ string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}
