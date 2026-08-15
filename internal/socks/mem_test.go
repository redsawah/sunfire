package socks

import (
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
)

// заглушка: соединение до цели, которое ничего не делает
type nopConn struct{ ch chan struct{} }

func (c nopConn) Read([]byte) (int, error) { <-c.ch; return 0, io.EOF }
func (c nopConn) Write(p []byte) (int, error) {
	return len(p), nil
}
func (c nopConn) Close() error { return nil }

type nopDialer struct{ ch chan struct{} }

func (d nopDialer) DialTarget(string) (io.ReadWriteCloser, error) {
	return nopConn{d.ch}, nil
}

// Сколько памяти держит простаивающая SOCKS-сессия. Считаются ОБА конца пары
// на петле — и клиентский сокет теста, и принятая сессия, — поэтому число выше
// того, что тратит один только сервер. Тест сторожит от разрастания буферов.
func TestSessionMemory(t *testing.T) {
	const n = 300
	stop := make(chan struct{})
	defer close(stop)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &Server{Dialer: nopDialer{stop}}
	go srv.Serve(ln)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var wg sync.WaitGroup
	conns := make([]net.Conn, 0, n)
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				return
			}
			// рукопожатие SOCKS5 без авторизации и CONNECT к домену
			c.Write([]byte{5, 1, 0})
			io.ReadFull(c, make([]byte, 2))
			req := []byte{5, 1, 0, 3, 11}
			req = append(req, []byte("example.com")...)
			req = append(req, 0x01, 0xbb)
			c.Write(req)
			io.ReadFull(c, make([]byte, 10))
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}()
	}
	wg.Wait()

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	if len(conns) == 0 {
		t.Fatal("ни одна сессия не установилась")
	}
	per := (after.HeapAlloc - before.HeapAlloc) / uint64(len(conns))
	t.Logf("на простаивающую сессию: %d КиБ (сессий %d)", per/1024, len(conns))
	runtime.KeepAlive(conns)
	for _, c := range conns {
		c.Close()
	}
	if per > 128*1024 {
		t.Fatalf("сессия держит %d КиБ, при десятках тысяч это гигабайты", per/1024)
	}
}
