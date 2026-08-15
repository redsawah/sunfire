package proto

import (
	"io"
	"net"
	"runtime"
	"testing"
)

// Сколько памяти удерживает простаивающее соединение. При десятках тысяч
// сессий именно это определяет расход, а не пропускная способность.
func TestIdleConnMemory(t *testing.T) {
	const n = 500

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	payload := make([]byte, MaxPayload)
	conns := make([]*Conn, 0, n)
	pipes := make([]net.Conn, 0, 2*n)
	for i := 0; i < n; i++ {
		cli, srv := net.Pipe()
		pipes = append(pipes, cli, srv)
		go func() { _, _ = io.Copy(io.Discard, srv) }()
		c, err := Dial(cli, "s3cr3t-token-длинный", "example.com:443")
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		// полноразмерный кадр, как при настоящей передаче, дальше простой
		if _, err := c.Write(payload); err != nil {
			t.Fatalf("Write: %v", err)
		}
		conns = append(conns, c)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	per := (after.HeapAlloc - before.HeapAlloc) / n
	t.Logf("на простаивающее соединение: %d КиБ", per/1024)
	runtime.KeepAlive(conns)
	for _, p := range pipes {
		p.Close()
	}
	if per > 64*1024 {
		t.Fatalf("соединение держит %d КиБ, это слишком много при десятках тысяч сессий", per/1024)
	}
}
