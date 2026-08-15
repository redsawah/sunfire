package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"sunfire/internal/config"
	"sunfire/internal/probe"
	"sunfire/internal/proto"
	"sunfire/internal/transport"
)

const (
	handshakeTimeout = 15 * time.Second
	dialTimeout      = 20 * time.Second
	bufSize          = 64 * 1024
)

func main() {
	cfgPath := flag.String("config", "/etc/sunfire/main.conf", "путь к конфигу")
	newToken := flag.Bool("token", false, "напечатать новый токен и выйти")
	flag.Parse()

	if *newToken {
		t, err := config.NewToken()
		if err != nil {
			log.Fatalf("sunfired: %v", err)
		}
		fmt.Println(t)
		return
	}

	if err := run(*cfgPath); err != nil {
		log.Fatalf("sunfired: %v", err)
	}
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	tokens := cfg.Tokens()

	var wg sync.WaitGroup
	errc := make(chan error, len(cfg.Listen))
	for name, addr := range cfg.Listen {
		t, err := transport.Get(name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		ln, err := t.Listen(addr, firstToken(tokens))
		if err != nil {
			return fmt.Errorf("слушать %s %s: %w", name, addr, err)
		}
		defer ln.Close()
		log.Printf("%s %s", name, addr)

		wg.Add(1)
		go func(name string, ln net.Listener) {
			defer wg.Done()
			errc <- serve(ln, cfg, tokens)
		}(name, ln)
	}
	if len(cfg.Listen) == 0 {
		return errors.New("нечего слушать")
	}
	err = <-errc
	wg.Wait()
	return err
}

// firstToken — ключ транспортного слоя. Все клиенты этого шлюза используют
// общий транспортный ключ, а различаются токеном внутри потока.
func firstToken(tokens map[string]string) string {
	for t := range tokens {
		return t
	}
	return ""
}

func serve(ln net.Listener, cfg config.Config, tokens map[string]string) error {
	for {
		raw, err := ln.Accept()
		if err != nil {
			return err
		}
		go handle(raw, cfg, tokens)
	}
}

func handle(raw net.Conn, cfg config.Config, tokens map[string]string) {
	defer raw.Close()

	_ = raw.SetDeadline(time.Now().Add(handshakeTimeout))
	conn, target, user, err := proto.Accept(raw, tokens)
	if err != nil {
		return
	}
	_ = raw.SetDeadline(time.Time{})

	if !cfg.Allowed(user, remoteIP(raw)) {
		log.Printf("отказ %s: адрес не разрешён", user)
		return
	}

	if target == probe.Target {
		sink(conn)
		return
	}

	out, err := net.DialTimeout("tcp", target, dialTimeout)
	if err != nil {
		return
	}
	defer out.Close()
	splice(conn, out)
}

// sink обслуживает замер: отражает первую посылку и дальше поглощает поток.
func sink(conn *proto.Conn) {
	buf := make([]byte, bufSize)
	n, err := conn.Read(buf)
	if err != nil {
		return
	}
	if _, err := conn.Write(buf[:n]); err != nil {
		return
	}
	_, _ = io.CopyBuffer(io.Discard, conn, buf)
}

func splice(a *proto.Conn, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, bufSize)
		_, _ = io.CopyBuffer(b, a, buf)
		if tc, ok := b.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, bufSize)
		_, _ = io.CopyBuffer(a, b, buf)
	}()
	wg.Wait()
}

func remoteIP(c net.Conn) net.IP {
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}
