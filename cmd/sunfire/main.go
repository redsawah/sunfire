package main

import (
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"strings"
	"time"

	"sunfire/internal/probe"
	"sunfire/internal/proto"
	"sunfire/internal/socks"
	"sunfire/internal/transport"
)

const dialTimeout = 10 * time.Second

func main() {
	gw := flag.String("gw", "", "адрес шлюза host:port")
	token := flag.String("token", "", "токен")
	socksAddr := flag.String("socks", "127.0.0.1:1080", "адрес локального SOCKS5")
	tr := flag.String("transport", "auto", "транспорт: auto|tcp|udp")
	probeTime := flag.Duration("probe-time", 3*time.Second, "длительность замера одного транспорта")
	verbose := flag.Bool("v", false, "подробный лог")
	flag.Parse()

	if err := run(*gw, *token, *socksAddr, *tr, *probeTime, *verbose); err != nil {
		log.Fatalf("sunfire: %v", err)
	}
}

func run(gw, token, socksAddr, tr string, probeTime time.Duration, verbose bool) error {
	if strings.TrimSpace(gw) == "" || strings.TrimSpace(token) == "" {
		return errors.New("нужны -gw и -token")
	}
	if err := requireLoopback(socksAddr); err != nil {
		return err
	}
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
	log.Printf("SOCKS5 %s -> %s (%s)", socksAddr, gw, name)

	srv := &socks.Server{
		Dialer: &gwDialer{tr: t, addr: gw, token: token},
		Logf:   logf,
	}
	return srv.Serve(ln)
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
	tr    transport.Transport
	addr  string
	token string
}

func (d *gwDialer) DialTarget(target string) (io.ReadWriteCloser, error) {
	raw, err := d.tr.Dial(d.addr, d.token, dialTimeout)
	if err != nil {
		return nil, err
	}
	c, err := proto.Dial(raw, d.token, target)
	if err != nil {
		raw.Close()
		return nil, err
	}
	return c, nil
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
