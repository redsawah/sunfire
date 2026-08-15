package failover

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"sunfire/internal/clientconf"
)

const token = "0123456789abcdefghij"

// fake — подставной Dialer: помнит, кто мёртв, и считает пробы.
type fake struct {
	mu    sync.Mutex
	dead  map[string]bool
	calls map[string]int
	delay time.Duration
}

func newFake(dead ...string) *fake {
	f := &fake{dead: map[string]bool{}, calls: map[string]int{}}
	for _, name := range dead {
		f.dead[name] = true
	}
	return f
}

func (f *fake) Probe(srv clientconf.Server, _ string, _ time.Duration) (time.Duration, error) {
	f.mu.Lock()
	f.calls[srv.Name]++
	dead, delay := f.dead[srv.Name], f.delay
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if dead {
		return 0, errors.New("нет ответа")
	}
	return time.Millisecond, nil
}

func (f *fake) set(name string, dead bool) {
	f.mu.Lock()
	f.dead[name] = dead
	f.mu.Unlock()
}

func (f *fake) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func servers(names ...string) []clientconf.Server {
	out := make([]clientconf.Server, 0, len(names))
	for i, name := range names {
		out = append(out, clientconf.Server{Name: name, GW: name + ".example:9000", Token: token + string(rune('a'+i))})
	}
	return out
}

// log — счётчик строк лога, безопасный для одновременных вызовов.
type log struct {
	mu    sync.Mutex
	lines []string
}

func (l *log) f(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, format)
}

func (l *log) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func TestPickPervyyZhivoy(t *testing.T) {
	f := newFake()
	p := New(servers("ams", "fra", "waw"), "tcp", nil, f)

	got, err := p.Pick(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "ams" {
		t.Fatalf("выбрали %q, ждали ams", got.Name)
	}
	if p.Current().Name != "ams" {
		t.Fatalf("текущий %q", p.Current().Name)
	}
	if f.count("fra") != 0 {
		t.Fatalf("зря трогали fra: %d проб", f.count("fra"))
	}
}

func TestPickPropuskaetMertvogo(t *testing.T) {
	f := newFake("ams")
	p := New(servers("ams", "fra"), "tcp", nil, f)

	got, err := p.Pick(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "fra" {
		t.Fatalf("выбрали %q, ждали fra", got.Name)
	}
}

func TestCurrentDoVybora(t *testing.T) {
	p := New(servers("ams"), "tcp", nil, newFake())
	if p.Current() != (clientconf.Server{}) {
		t.Fatalf("до Pick текущий %+v", p.Current())
	}
}

func TestFailedPereklyuchaet(t *testing.T) {
	f := newFake()
	lg := &log{}
	p := New(servers("ams", "fra"), "tcp", lg.f, f)

	if _, err := p.Pick(time.Second); err != nil {
		t.Fatal(err)
	}
	f.set("ams", true)

	got, err := failN(t, p, failThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "fra" || p.Current().Name != "fra" {
		t.Fatalf("после отказа %q, текущий %q", got.Name, p.Current().Name)
	}
	if lg.count("переключаюсь") != 1 {
		t.Fatalf("переключений в логе %d, ждали 1", lg.count("переключаюсь"))
	}
}

func TestFailedZhivogoNeTrogaet(t *testing.T) {
	f := newFake()
	p := New(servers("ams", "fra"), "tcp", nil, f)

	if _, err := p.Pick(time.Second); err != nil {
		t.Fatal(err)
	}
	got, err := p.Failed(errors.New("случайный обрыв"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "ams" {
		t.Fatalf("ушли на %q, а ams жив", got.Name)
	}
	if f.count("fra") != 0 {
		t.Fatal("зря пробовали fra")
	}
}

func TestFailedOnChuzhogoNeTrogaet(t *testing.T) {
	f := newFake()
	p := New(servers("ams", "fra"), "tcp", nil, f)

	if _, err := p.Pick(time.Second); err != nil {
		t.Fatal(err)
	}
	stale := clientconf.Server{Name: "waw"}
	got, err := p.FailedOn(stale, errors.New("обрыв"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "ams" {
		t.Fatalf("получили %q, ждали ams", got.Name)
	}
	if f.count("ams") != 1 {
		t.Fatalf("проб ams %d, ждали 1", f.count("ams"))
	}
}

func TestFailedOdnovremenno(t *testing.T) {
	f := newFake("ams")
	f.delay = 2 * time.Millisecond // чтобы вызовы успели столкнуться
	lg := &log{}
	p := New(servers("ams", "fra", "waw"), "tcp", lg.f, f)

	// садимся на ams, пока он ещё отвечает
	f.set("ams", false)
	if _, err := p.Pick(time.Second); err != nil {
		t.Fatal(err)
	}
	f.set("ams", true)

	const n = 50
	got := make([]clientconf.Server, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = failN(t, p, failThreshold)
		}()
	}
	wg.Wait()

	for i := range got {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if got[i].Name != "fra" {
			t.Fatalf("goroutine %d получила %q, ждали fra", i, got[i].Name)
		}
	}
	if p.Current().Name != "fra" {
		t.Fatalf("текущий %q", p.Current().Name)
	}
	if lg.count("переключаюсь") != 1 {
		t.Fatalf("переключений %d, ждали 1", lg.count("переключаюсь"))
	}
	if f.count("waw") != 0 {
		t.Fatalf("докатились до waw: %d проб", f.count("waw"))
	}
}

func TestCooldown(t *testing.T) {
	f := newFake()
	p := New(servers("ams", "fra"), "tcp", nil, f)
	clock := time.Now()
	p.now = func() time.Time { return clock }

	if _, err := p.Pick(time.Second); err != nil {
		t.Fatal(err)
	}
	f.set("ams", true)
	if _, err := failN(t, p, failThreshold); err != nil {
		t.Fatal(err)
	}
	f.set("ams", false) // поднялся, но остывает

	was := f.count("ams")
	got, err := p.Pick(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "fra" {
		t.Fatalf("вернулись на %q раньше времени", got.Name)
	}
	if f.count("ams") != was {
		t.Fatal("трогали ams во время остывания")
	}

	clock = clock.Add(cooldown + time.Second)
	got, err = p.Pick(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "ams" {
		t.Fatalf("после остывания выбрали %q, ждали ams", got.Name)
	}
}

func TestVseMertvy(t *testing.T) {
	f := newFake("ams", "fra")
	p := New(servers("ams", "fra"), "tcp", nil, f)

	_, err := p.Pick(time.Second)
	if err == nil {
		t.Fatal("ждали ошибку")
	}
	if !strings.HasPrefix(err.Error(), "sunfire: ") {
		t.Fatalf("ошибка без префикса: %v", err)
	}

	// все на остывании, но пробовать всё равно нужно
	if _, err := p.Pick(time.Second); err == nil {
		t.Fatal("ждали ошибку")
	}
	if f.count("ams") != 2 || f.count("fra") != 2 {
		t.Fatalf("проб ams %d, fra %d, ждали по 2", f.count("ams"), f.count("fra"))
	}

	// живых нет — Failed тоже с ошибкой, текущего не появилось
	if _, err := p.Failed(errors.New("обрыв"), time.Second); err == nil {
		t.Fatal("ждали ошибку")
	}
	if p.Current() != (clientconf.Server{}) {
		t.Fatalf("текущий %+v", p.Current())
	}
}

func TestPustoySpisok(t *testing.T) {
	p := New(nil, "tcp", nil, newFake())
	if _, err := p.Pick(time.Second); err == nil {
		t.Fatal("ждали ошибку")
	}
}

// failN доводит пул до порога отказа: одиночная неудача сервер не меняет.
func failN(t *testing.T, p *Pool, n int) (clientconf.Server, error) {
	t.Helper()
	var srv clientconf.Server
	var err error
	for i := 0; i < n; i++ {
		srv, err = p.Failed(errors.New("обрыв"), time.Second)
	}
	return srv, err
}

// Одна потеря пакета не должна уводить с живого сервера: на нагруженном плече
// отдельное подключение регулярно не укладывается в таймаут.
func TestOdinochnayaNeudachaNeMenyaetServer(t *testing.T) {
	lg := &log{}
	d := newFake()
	p := New(servers("ams", "fra"), "tcp", lg.f, d)
	if _, err := p.Pick(time.Second); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < failThreshold-1; i++ {
		got, err := p.Failed(errors.New("обрыв"), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "ams" || p.Current().Name != "ams" {
			t.Fatalf("после %d неудач ушли на %q, а сервер живой", i+1, got.Name)
		}
	}
	if lg.count("переключаюсь") != 0 {
		t.Fatalf("переключились раньше порога: %d", lg.count("переключаюсь"))
	}
	// удачное подключение обнуляет череду
	p.OK()
	for i := 0; i < failThreshold-1; i++ {
		if _, err := p.Failed(errors.New("обрыв"), time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if p.Current().Name != "ams" {
		t.Fatal("после успеха счётчик не обнулился")
	}
}
