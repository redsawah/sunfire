package transport

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net"
	"time"

	"github.com/xtaci/kcp-go/v5"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

type udpTransport struct{}

func (udpTransport) Name() string { return "udp" }

// Dial: дедлайны на сессии выставляет вызывающий — здесь их ставить нельзя,
// они остались бы на всё время соединения.
func (udpTransport) Dial(addr, token string, _ time.Duration) (net.Conn, error) {
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	aead, err := deriveAEAD(token)
	if err != nil {
		return nil, err
	}
	pc, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return nil, err
	}
	tuneSocket(pc)
	// ownConn=true: сессия закроет сокет сама.
	sess, err := kcp.NewConn4(0, raddr, nil, 0, 0, true, &sealedPacketConn{PacketConn: pc, aead: aead})
	if err != nil {
		pc.Close()
		return nil, err
	}
	tune(sess)
	return sess, nil
}

func (udpTransport) Listen(addr, token string) (net.Listener, error) {
	aead, err := deriveAEAD(token)
	if err != nil {
		return nil, err
	}
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	tuneSocket(pc)
	ln, err := kcp.ServeConn(nil, 0, 0, &sealedPacketConn{PacketConn: pc, aead: aead})
	if err != nil {
		pc.Close()
		return nil, err
	}
	return &kcpListener{Listener: ln, pc: pc}, nil
}

func deriveAEAD(token string) (cipher.AEAD, error) {
	r := hkdf.New(sha256.New, []byte(token), nil, []byte("sunfire/1 udp"))
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, err
	}
	return chacha20poly1305.New(key)
}

func tune(s *kcp.UDPSession) {
	s.SetStreamMode(true)
	s.SetNoDelay(1, 10, 2, 1)
	// окно под произведение полосы на задержку: 1024 пакета не покрывают
	// быстрый маршрут с заметным RTT
	s.SetWindowSize(4096, 4096)
	s.SetMtu(1350)
}

// sockBuf — буферы UDP-сокета. По умолчанию ядро даёт сотни килобайт, чего
// на гигабите не хватает и датаграммы теряются в очереди приёма.
const sockBuf = 8 << 20

func tuneSocket(pc net.PacketConn) {
	if u, ok := pc.(*net.UDPConn); ok {
		_ = u.SetReadBuffer(sockBuf)
		_ = u.SetWriteBuffer(sockBuf)
	}
}

// kcpListener применяет настройки сессии к каждому принятому соединению.
type kcpListener struct {
	*kcp.Listener
	pc net.PacketConn
}

func (l *kcpListener) Accept() (net.Conn, error) {
	s, err := l.AcceptKCP()
	if err != nil {
		return nil, err
	}
	tune(s)
	return s, nil
}

func (l *kcpListener) Close() error {
	err := l.Listener.Close()
	l.pc.Close()
	return err
}

// sealedPacketConn шифрует каждую датаграмму: [nonce 12][шифртекст+тег].
type sealedPacketConn struct {
	net.PacketConn
	aead cipher.AEAD
}

func (c *sealedPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return 0, err
	}
	if _, err := c.PacketConn.WriteTo(c.aead.Seal(nonce, nonce, p, nil), addr); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *sealedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	buf := make([]byte, 2048+c.aead.NonceSize()+c.aead.Overhead())
	ns := c.aead.NonceSize()
	for {
		n, addr, err := c.PacketConn.ReadFrom(buf)
		if err != nil {
			return 0, addr, err
		}
		if n < ns+c.aead.Overhead() {
			continue
		}
		plain, err := c.aead.Open(nil, buf[:ns], buf[ns:n], nil)
		if err != nil {
			continue // битую или чужую датаграмму молча пропускаем
		}
		return copy(p, plain), addr, nil
	}
}
