package transport

import (
	"io"
	"slices"
	"testing"
	"time"
)

const key = "0123456789abcdefghijklmn"

func TestAvailable(t *testing.T) {
	if got := Available(key); !slices.Equal(got, Names) {
		t.Fatalf("с ключом доступны %v", got)
	}
	got := Available("")
	if slices.Contains(got, "udp") {
		t.Fatalf("без ключа предложили udp: %v", got)
	}
	if len(got) == 0 {
		t.Fatal("без ключа не осталось ни одного транспорта")
	}
	if !NeedsKey("udp") || NeedsKey("tcp") {
		t.Fatal("не тем транспортам нужен ключ")
	}
}

// Ключ udp общий для шлюза и не выводится из токена пользователя: датаграмму
// приходится расшифровать раньше, чем станет известно, кто её прислал.
func TestUDPObshchiyKlyuch(t *testing.T) {
	tr := udpTransport{}
	ln, err := tr.Listen("127.0.0.1:0", key)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		io.Copy(c, c)
	}()

	c, err := tr.Dial(ln.Addr().String(), key, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))

	want := []byte("привет")
	if _, err := c.Write(want); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("вернулось %q", got)
	}
}
