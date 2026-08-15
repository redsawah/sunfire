// Package socks — локальный SOCKS5-вход клиента.
package socks

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// Dialer открывает поток до цели через шлюз.
type Dialer interface {
	DialTarget(target string) (io.ReadWriteCloser, error)
}

type Server struct {
	Dialer Dialer
	Logf   func(string, ...any)
}

const (
	handshakeTimeout = 30 * time.Second
	bufSize          = 64 * 1024

	ver        = 0x05
	cmdConnect = 0x01

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repOK      = 0x00
	repRefused = 0x05
	repBadCmd  = 0x07
	repBadAddr = 0x08
)

var errVersion = errors.New("sunfire: не SOCKS5")

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))

	if err := greet(c); err != nil {
		s.logf("socks: приветствие: %v", err)
		return
	}
	target, code, err := readRequest(c)
	if err != nil {
		s.logf("socks: запрос: %v", err)
		return
	}
	if code != repOK {
		_ = reply(c, code)
		return
	}

	dst, err := s.Dialer.DialTarget(target)
	if err != nil {
		s.logf("socks: %s: %v", target, err)
		_ = reply(c, repRefused)
		return
	}
	defer dst.Close()

	if err := reply(c, repOK); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	splice(c, dst)
}

func greet(c net.Conn) error {
	var head [2]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return err
	}
	if head[0] != ver {
		return errVersion
	}
	if _, err := io.ReadFull(c, make([]byte, head[1])); err != nil {
		return err
	}
	_, err := c.Write([]byte{ver, 0x00}) // без аутентификации
	return err
}

// readRequest разбирает CONNECT. Если запрос понятен, но не поддержан,
// возвращает код ответа и nil-ошибку.
func readRequest(c net.Conn) (string, byte, error) {
	var head [4]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return "", 0, err
	}
	if head[0] != ver {
		return "", 0, errVersion
	}
	if head[1] != cmdConnect {
		return "", repBadCmd, nil
	}

	var host string
	switch head[3] {
	case atypIPv4:
		var b [4]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return "", 0, err
		}
		host = net.IP(b[:]).String()
	case atypDomain:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return "", 0, err
		}
		b := make([]byte, n[0])
		if _, err := io.ReadFull(c, b); err != nil {
			return "", 0, err
		}
		host = string(b)
	case atypIPv6:
		var b [16]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return "", 0, err
		}
		host = net.IP(b[:]).String()
	default:
		return "", repBadAddr, nil
	}

	var p [2]byte
	if _, err := io.ReadFull(c, p[:]); err != nil {
		return "", 0, err
	}
	port := strconv.Itoa(int(binary.BigEndian.Uint16(p[:])))
	return net.JoinHostPort(host, port), repOK, nil
}

func reply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{ver, code, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

func splice(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(a, b, &wg)
	go pipe(b, a, &wg)
	wg.Wait()
}

func pipe(dst, src io.ReadWriteCloser, wg *sync.WaitGroup) {
	defer wg.Done()
	_, _ = io.CopyBuffer(dst, src, make([]byte, bufSize))
	if tc, ok := dst.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}
