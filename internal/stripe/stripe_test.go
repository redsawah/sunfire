package stripe_test

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"runtime"
	"strconv"
	"testing"
	"time"

	"sunfire/internal/stripe"
)

func pipes(n int) (a, b []io.ReadWriteCloser) {
	for range n {
		x, y := net.Pipe()
		a = append(a, x)
		b = append(b, y)
	}
	return a, b
}

func TestJoinRoundtrip(t *testing.T) {
	for _, n := range []int{1, 2, 4} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			data := make([]byte, 3*1024*1024+12345)
			if _, err := rand.Read(data); err != nil {
				t.Fatal(err)
			}
			a, b := pipes(n)
			sa, sb := stripe.Join(a), stripe.Join(b)

			werr := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				_, err := sa.Write(data)
				werr <- err
				sa.Close()
				close(done)
			}()

			got, err := io.ReadAll(sb)
			if err != nil {
				t.Fatalf("чтение: %v", err)
			}
			sb.Close()
			<-done

			if err := <-werr; err != nil {
				t.Fatalf("запись: %v", err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("получено %d байт из %d, данные не совпали", len(got), len(data))
			}
		})
	}
}

func TestJoinSmallWrites(t *testing.T) {
	a, b := pipes(3)
	sa, sb := stripe.Join(a), stripe.Join(b)

	var want []byte
	done := make(chan struct{})
	go func() {
		for i := range 500 {
			p := []byte(strconv.Itoa(i) + ";")
			want = append(want, p...)
			if _, err := sa.Write(p); err != nil {
				t.Error(err)
				break
			}
		}
		sa.Close()
		close(done)
	}()

	got, err := io.ReadAll(sb)
	if err != nil {
		t.Fatal(err)
	}
	sb.Close()
	<-done

	if !bytes.Equal(got, want) {
		t.Fatalf("порядок нарушен: %q", got)
	}
}

func TestHeads(t *testing.T) {
	sid, err := stripe.NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if len(sid) != 32 {
		t.Fatalf("sid %q", sid)
	}
	other, _ := stripe.NewSessionID()
	if sid == other {
		t.Fatal("sid повторился")
	}

	h, err := stripe.ParseHead(stripe.FrameStart(sid, 4, "example.org:443"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Kind != stripe.KindStart || h.SID != sid || h.Count != 4 || h.Target != "example.org:443" {
		t.Fatalf("start: %+v", h)
	}

	h, err = stripe.ParseHead(stripe.FrameJoin(sid, 3))
	if err != nil {
		t.Fatal(err)
	}
	if h.Kind != stripe.KindJoin || h.SID != sid || h.Index != 3 {
		t.Fatalf("join: %+v", h)
	}

	for _, s := range []string{"1.2.3.4:80", "[::1]:443", "@probe"} {
		h, err = stripe.ParseHead(s)
		if err != nil {
			t.Fatal(err)
		}
		if h.Kind != stripe.KindPlain || h.Target != s {
			t.Fatalf("plain %q: %+v", s, h)
		}
	}

	bad := []string{
		"@s " + sid + " 4",
		"@s " + sid + " x host:1",
		"@s zzz 4 host:1",
		"@s " + sid + " 0 host:1",
		"@j " + sid,
		"@j " + sid + " x",
		"@j " + sid + " 0",
	}
	for _, s := range bad {
		if _, err := stripe.ParseHead(s); err == nil {
			t.Fatalf("принят мусор %q", s)
		}
	}
}

// CloseWrite завершает своё направление, обратное продолжает работать.
func TestCloseWrite(t *testing.T) {
	a, b := pipes(3)
	sa, sb := stripe.Join(a), stripe.Join(b)
	defer sa.Close()
	defer sb.Close()

	half, ok := sa.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("раскладка не умеет CloseWrite")
	}

	go func() {
		sa.Write([]byte("вопрос"))
		half.CloseWrite()
	}()

	got, err := io.ReadAll(sb)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "вопрос" {
		t.Fatalf("получено %q", got)
	}
	if _, err := sa.Write([]byte("ещё")); err == nil {
		t.Fatal("запись после CloseWrite прошла")
	}

	// обратное направление живо
	go func() {
		sb.Write([]byte("ответ"))
		sb.(interface{ CloseWrite() error }).CloseWrite()
	}()
	got, err = io.ReadAll(sa)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ответ" {
		t.Fatalf("получено %q", got)
	}
}

func TestCloseNoLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	for range 10 {
		a, b := pipes(4)
		sa, sb := stripe.Join(a), stripe.Join(b)

		done := make(chan struct{})
		go func() {
			sa.Write(bytes.Repeat([]byte("данные"), 4096))
			sa.Close()
			sa.Close() // повторный Close безопасен
			close(done)
		}()

		if _, err := io.ReadAll(sb); err != nil {
			t.Fatal(err)
		}
		sb.Close()
		sb.Close()
		<-done
	}

	// даём планировщику доработать
	var now int
	for range 200 {
		now = runtime.NumGoroutine()
		if now <= before+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if now > before+2 {
		t.Fatalf("goroutine: было %d, стало %d", before, now)
	}
}
