package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"sunfire/internal/probe"
	"sunfire/internal/proto"
	"sunfire/internal/selfupdate"
	"sunfire/internal/socks"
	"sunfire/internal/stats"
	"sunfire/internal/stripe"
	"sunfire/internal/transport"
	"sunfire/internal/tui"
	"sunfire/internal/version"
)

const (
	appName     = "sunfire"
	dialTimeout = 10 * time.Second
)

func main() {
	gw := flag.String("gw", "", "адрес шлюза host:port")
	token := flag.String("token", "", "токен")
	socksAddr := flag.String("socks", "127.0.0.1:1080", "адрес локального SOCKS5")
	tr := flag.String("transport", "auto", "транспорт: auto|tcp|udp")
	probeTime := flag.Duration("probe-time", 3*time.Second, "длительность замера одного транспорта")
	streams := flag.Int("streams", 1, "соединений на сессию")
	verbose := flag.Bool("v", false, "подробный лог")
	showVersion := flag.Bool("version", false, "напечатать версию и выйти")
	update := flag.Bool("update", false, "обновиться до последней версии и выйти")
	checkUpdate := flag.Bool("check-update", false, "проверить наличие новой версии и выйти")
	statusAddr := flag.String("status", "127.0.0.1:9602", "адрес статуса для монитора, только петля")
	top := flag.Bool("top", false, "открыть монитор и выйти")
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println(version.Number)
		return
	case *update, *checkUpdate:
		if err := selfupdate.Run(appName, version.Number, *checkUpdate, log.Printf); err != nil {
			log.Fatal(err)
		}
		return
	case *top:
		if err := tui.Run(*statusAddr); err != nil {
			log.Fatal(err)
		}
		return
	}

	if err := run(*gw, *token, *socksAddr, *statusAddr, *tr, *probeTime, *streams, *verbose); err != nil {
		log.Fatalf("sunfire: %v", err)
	}
}

func run(gw, token, socksAddr, statusAddr, tr string, probeTime time.Duration, streams int, verbose bool) error {
	if strings.TrimSpace(gw) == "" || strings.TrimSpace(token) == "" {
		return errors.New("нужны -gw и -token")
	}
	if streams < 1 {
		streams = 1
	}
	if err := requireLoopback(socksAddr); err != nil {
		return err
	}
	if err := requireLoopback(statusAddr); err != nil {
		return err
	}
	stats.HookLog(os.Stderr, 200)
	logf := func(format string, args ...any) {
		if verbose {
			log.Printf(format, args...)
		}
	}

	name := tr
	if name == "auto" {
		var err error
		name, err = pick(gw, token, probeTime, logf)
		if err != nil {
			return err
		}
	}
	t, err := transport.Get(name)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", socksAddr)
	if err != nil {
		return err
	}
	defer ln.Close()

	reg := stats.New()
	extra := gw + " · " + name
	if streams > 1 {
		extra += fmt.Sprintf(" · соединений на сессию: %d", streams)
	}
	reg.SetMeta("клиент", version.Number, extra)
	serveStatus(statusAddr, reg)

	log.Printf("SOCKS5 %s -> %s (%s)", socksAddr, gw, name)
	if streams > 1 {
		log.Printf("соединений на сессию: %d", streams)
	}
	log.Printf("монитор: sunfire -top")

	srv := &socks.Server{
		Dialer: &gwDialer{tr: t, addr: gw, token: token, streams: streams, reg: reg},
		Logf:   logf,
	}
	return srv.Serve(ln)
}

// serveStatus поднимает статус для монитора. Только петля.
func serveStatus(addr string, reg *stats.Reg) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reg.Snapshot())
	})
	s := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := s.ListenAndServe(); err != nil {
			log.Printf("статус не поднялся: %v", err)
		}
	}()
}

// pick замеряет транспорты и берёт самый быстрый.
func pick(gw, token string, d time.Duration, logf func(string, ...any)) (string, error) {
	log.Printf("замер транспортов…")
	results := probe.Run(probe.Options{
		Addr:     gw,
		Token:    token,
		Duration: d,
		Timeout:  dialTimeout,
		Logf:     logf,
	})
	for _, r := range results {
		if r.Err != nil {
			log.Printf("  %-4s ошибка: %v", r.Transport, r.Err)
			continue
		}
		log.Printf("  %-4s %.0f Мбит/с, rtt %v", r.Transport, r.Mbps, r.RTT.Round(time.Millisecond))
	}
	return probe.Best(results)
}

type gwDialer struct {
	tr      transport.Transport
	addr    string
	token   string
	streams int
	reg     *stats.Reg
}

// counted считает трафик сессии; закрытие снимает её со счётчика.
type counted struct {
	io.ReadWriteCloser
	c *stats.Conn
}

func (c counted) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	c.c.Down(int64(n))
	return n, err
}

func (c counted) Write(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Write(p)
	c.c.Up(int64(n))
	return n, err
}

func (c counted) Close() error {
	c.c.Close()
	return c.ReadWriteCloser.Close()
}

// CloseWrite пробрасываем дальше: без него конец потока не доезжает до второй
// стороны и соединения висят до обрыва.
func (c counted) CloseWrite() error {
	if w, ok := c.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return nil
}

func (d *gwDialer) DialTarget(target string) (io.ReadWriteCloser, error) {
	rw, err := d.dialTarget(target)
	if err != nil {
		return nil, err
	}
	host, _, e := net.SplitHostPort(target)
	if e != nil {
		host = target
	}
	return counted{rw, d.reg.Open(host)}, nil
}

func (d *gwDialer) dialTarget(target string) (io.ReadWriteCloser, error) {
	if d.streams < 2 {
		return d.dial(target)
	}
	sid, err := stripe.NewSessionID()
	if err != nil {
		return nil, err
	}

	// соединения раскладки открываем разом: последовательно ушёл бы лишний RTT на каждое
	conns := make([]io.ReadWriteCloser, d.streams)
	errs := make([]error, d.streams)
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			head := stripe.FrameJoin(sid, i)
			if i == 0 {
				head = stripe.FrameStart(sid, d.streams, target)
			}
			conns[i], errs[i] = d.dial(head)
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			for _, c := range conns {
				if c != nil {
					c.Close()
				}
			}
			return nil, err
		}
	}
	return stripe.Join(conns), nil
}

func (d *gwDialer) dial(head string) (io.ReadWriteCloser, error) {
	raw, err := d.tr.Dial(d.addr, d.token, dialTimeout)
	if err != nil {
		return nil, err
	}
	c, err := proto.Dial(raw, d.token, head)
	if err != nil {
		raw.Close()
		return nil, err
	}
	return halfConn{Conn: c, raw: raw}, nil
}

// halfConn доносит до шлюза конец своей половины потока, не закрывая обратную.
// Транспорт без половинного закрытия (udp) обходится без этого.
type halfConn struct {
	*proto.Conn
	raw net.Conn
}

func (c halfConn) CloseWrite() error {
	if w, ok := c.raw.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return nil
}

// requireLoopback — SOCKS без авторизации, наружу его выставлять нельзя.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	return errors.New("адрес SOCKS обязан быть петлёй")
}
