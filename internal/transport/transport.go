package transport

import (
	"errors"
	"net"
	"time"
)

// Transport — способ добраться до шлюза. Даёт надёжный поток байт, поверх
// которого работает internal/proto.
type Transport interface {
	Name() string
	Dial(addr, token string, timeout time.Duration) (net.Conn, error)
	Listen(addr, token string) (net.Listener, error)
}

var ErrUnknown = errors.New("transport: неизвестный транспорт")

var registry = map[string]Transport{
	"tcp": tcpTransport{},
	"udp": udpTransport{},
}

// Names — список доступных транспортов в порядке предпочтения по умолчанию.
var Names = []string{"tcp", "udp"}

func Get(name string) (Transport, error) {
	t, ok := registry[name]
	if !ok {
		return nil, ErrUnknown
	}
	return t, nil
}
