package stats_test

import (
	"bytes"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"sunfire/internal/stats"
)

func TestParallel(t *testing.T) {
	const (
		workers = 64
		rounds  = 200
		names   = 8
	)
	r := stats.New()

	stop := make(chan struct{})
	var mon sync.WaitGroup
	mon.Add(1)
	go func() { // монитор дёргает Snapshot параллельно с нагрузкой
		defer mon.Done()
		for {
			select {
			case <-stop:
				return
			default:
				r.Snapshot()
			}
		}
	}()

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := "u" + strconv.Itoa(i%names)
			for range rounds {
				c := r.Open(name)
				c.Up(3)
				c.Down(5)
				c.Up(-1) // отрицательное не учитывается
				c.Close()
				c.Close() // повторный Close безвреден
			}
		}()
	}
	wg.Wait()
	close(stop)
	mon.Wait()

	s := r.Snapshot()
	if len(s.Users) != names {
		t.Fatalf("пользователей %d, ждали %d", len(s.Users), names)
	}
	var total int64
	var up, down uint64
	for _, u := range s.Users {
		if u.Conns != 0 {
			t.Errorf("%s: осталось %d активных", u.Name, u.Conns)
		}
		total += u.Total
		up += u.Up
		down += u.Down
	}
	if want := int64(workers * rounds); total != want {
		t.Errorf("всего %d, ждали %d", total, want)
	}
	if want := uint64(workers * rounds * 3); up != want {
		t.Errorf("up %d, ждали %d", up, want)
	}
	if want := uint64(workers * rounds * 5); down != want {
		t.Errorf("down %d, ждали %d", down, want)
	}
}

func TestSnapshot(t *testing.T) {
	r := stats.New()
	r.SetMeta("сервер", "0.2.0", ":443, :8443")

	c := r.Open("иван")
	c.Up(100)
	c.Down(900)

	d := r.Open("пётр")
	d.Up(10)
	d.Down(10)
	d.Close()

	e := r.Open("пётр")
	e.Up(1)

	s := r.Snapshot()
	if s.Role != "сервер" || s.Version != "0.2.0" || s.Extra != ":443, :8443" {
		t.Fatalf("мета: %+v", s)
	}
	if s.Uptime < 0 {
		t.Fatalf("uptime %d", s.Uptime)
	}
	if len(s.Users) != 2 {
		t.Fatalf("пользователей %d", len(s.Users))
	}
	// сортировка по Up+Down по убыванию
	if s.Users[0].Name != "иван" || s.Users[1].Name != "пётр" {
		t.Fatalf("порядок: %+v", s.Users)
	}
	if s.Users[0] != (stats.UserStat{Name: "иван", Conns: 1, Total: 1, Up: 100, Down: 900}) {
		t.Fatalf("иван: %+v", s.Users[0])
	}
	if s.Users[1] != (stats.UserStat{Name: "пётр", Conns: 1, Total: 2, Up: 11, Down: 10}) {
		t.Fatalf("пётр: %+v", s.Users[1])
	}

	c.Close()
	e.Close()
	for _, u := range r.Snapshot().Users {
		if u.Conns != 0 {
			t.Fatalf("%s: %d активных", u.Name, u.Conns)
		}
	}
}

func TestCount(t *testing.T) {
	var n int64
	add := func(k int64) { n += k }

	src := strings.NewReader("привет")
	if _, err := io.Copy(io.Discard, stats.CountReader(src, add)); err != nil {
		t.Fatal(err)
	}
	if want := int64(len("привет")); n != want {
		t.Fatalf("прочитано %d, ждали %d", n, want)
	}

	n = 0
	var buf bytes.Buffer
	w := stats.CountWriter(&buf, add)
	w.Write([]byte("раз"))
	w.Write([]byte("два"))
	if want := int64(len("раздва")); n != want {
		t.Fatalf("записано %d, ждали %d", n, want)
	}
	if buf.String() != "раздва" {
		t.Fatalf("буфер %q", buf.String())
	}
}

func TestRing(t *testing.T) {
	defer log.SetOutput(os.Stderr)
	defer log.SetFlags(log.LstdFlags)
	log.SetFlags(0)

	var out bytes.Buffer
	stats.HookLog(&out, 3)

	if got := stats.LogLines(); len(got) != 0 {
		t.Fatalf("пустое кольцо: %q", got)
	}

	log.Print("один")
	log.Print("два")
	if got := stats.LogLines(); len(got) != 2 || got[0] != "один" || got[1] != "два" {
		t.Fatalf("недобор: %q", got)
	}

	for i := range 10 {
		log.Print("строка ", i)
	}
	got := stats.LogLines()
	want := []string{"строка 7", "строка 8", "строка 9"}
	if len(got) != len(want) {
		t.Fatalf("после переполнения %d строк: %q", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("строка %d: %q, ждали %q", i, got[i], want[i])
		}
	}

	// в w ушло всё
	if lines := strings.Count(out.String(), "\n"); lines != 12 {
		t.Fatalf("в поток ушло %d строк", lines)
	}

	// многострочное сообщение кладётся построчно, без перевода строки
	log.Print("а\nб")
	got = stats.LogLines()
	if got[1] != "а" || got[2] != "б" {
		t.Fatalf("многострочное: %q", got)
	}
	for _, s := range got {
		if strings.HasSuffix(s, "\n") {
			t.Fatalf("строка с переводом: %q", s)
		}
	}

	// LogLines отдаёт копию
	got[0] = "испорчено"
	if stats.LogLines()[0] == "испорчено" {
		t.Fatal("кольцо отдаёт свой буфер")
	}
}

func TestRingParallel(t *testing.T) {
	defer log.SetOutput(os.Stderr)
	stats.HookLog(io.Discard, 16)

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				log.Printf("поток %d шаг %d", i, j)
				stats.LogLines()
			}
		}()
	}
	wg.Wait()

	if got := stats.LogLines(); len(got) != 16 {
		t.Fatalf("в кольце %d строк", len(got))
	}
}
