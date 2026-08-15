package proto

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"testing"
)

// pair поднимает клиента и шлюз на net.Pipe и возвращает оба конца установленного
// соединения sunfire/1 плюс адрес, который увидел шлюз.
func pair(t *testing.T, token, target string, tokens map[string]string) (*Conn, *Conn, string) {
	t.Helper()
	cliRaw, srvRaw := net.Pipe()

	type accepted struct {
		conn   *Conn
		target string
		user   string
		err    error
	}
	done := make(chan accepted, 1)
	go func() {
		c, tgt, user, err := Accept(srvRaw, tokens)
		done <- accepted{c, tgt, user, err}
	}()

	cli, err := Dial(cliRaw, token, target)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	a := <-done
	if a.err != nil {
		t.Fatalf("Accept: %v", a.err)
	}
	return cli, a.conn, a.target
}

func TestRoundTrip(t *testing.T) {
	tokens := map[string]string{"s3cr3t": "node-a"}
	cli, srv, target := pair(t, "s3cr3t", "example.com:443", tokens)
	defer cli.Close()
	defer srv.Close()

	if target != "example.com:443" {
		t.Fatalf("адрес назначения: got %q, want %q", target, "example.com:443")
	}

	// клиент → шлюз
	go func() { _, _ = cli.Write([]byte("привет, шлюз")) }()
	buf := make([]byte, 64)
	n, err := srv.Read(buf)
	if err != nil {
		t.Fatalf("чтение на шлюзе: %v", err)
	}
	if got := string(buf[:n]); got != "привет, шлюз" {
		t.Fatalf("c→s: got %q", got)
	}

	// шлюз → клиент
	go func() { _, _ = srv.Write([]byte("привет, клиент")) }()
	n, err = cli.Read(buf)
	if err != nil {
		t.Fatalf("чтение на клиенте: %v", err)
	}
	if got := string(buf[:n]); got != "привет, клиент" {
		t.Fatalf("s→c: got %q", got)
	}
}

// TestLargePayload проверяет нарезку на кадры: объём заведомо больше MaxPayload,
// значит writeFrame обязан разложить его на несколько кадров, а Read — собрать
// обратно байт в байт.
func TestLargePayload(t *testing.T) {
	tokens := map[string]string{"s3cr3t": "node-a"}
	cli, srv, _ := pair(t, "s3cr3t", "example.com:443", tokens)
	defer cli.Close()
	defer srv.Close()

	payload := make([]byte, 3*MaxPayload+1234)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}

	go func() { _, _ = cli.Write(payload) }()

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(srv, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("данные разъехались при нарезке на кадры")
	}
}

// TestWrongToken: шлюз не знает такого токена — первый кадр не расшифровывается
// ни одним из известных, соединение должно быть отвергнуто.
func TestWrongToken(t *testing.T) {
	cliRaw, srvRaw := net.Pipe()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := Accept(srvRaw, map[string]string{"правильный": "u"})
		done <- err
	}()
	go func() { _, _ = Dial(cliRaw, "чужой", "example.com:443") }()

	if err := <-done; err != ErrUnknownToken {
		t.Fatalf("ожидался ErrUnknownToken, получено: %v", err)
	}
}

// TestSaltIsFresh: две сессии с одним токеном обязаны иметь разные соли, иначе
// совпадут ключи и счётчики-нонсы — а это полный слом AEAD.
func TestSaltIsFresh(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		cliRaw, srvRaw := net.Pipe()
		saltCh := make(chan []byte, 1)
		go func() {
			salt := make([]byte, saltLen)
			_, _ = io.ReadFull(srvRaw, salt)
			saltCh <- salt
			_, _ = io.Copy(io.Discard, srvRaw)
		}()
		go func() { _, _ = Dial(cliRaw, "s3cr3t", "example.com:443") }()
		salt := string(<-saltCh)
		if seen[salt] {
			t.Fatal("соль повторилась между сессиями")
		}
		seen[salt] = true
		cliRaw.Close()
		srvRaw.Close()
	}
}
