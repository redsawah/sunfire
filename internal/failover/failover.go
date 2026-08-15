// Package failover — выбор живого сервера из списка и переключение при отказе.
package failover

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"sunfire/internal/clientconf"
	"sunfire/internal/probe"
	"sunfire/internal/proto"
	"sunfire/internal/transport"
)

// cooldown — сколько отказавший сервер не пробуем.
const cooldown = 60 * time.Second

const rttPayload = 32

type Dialer interface {
	// Probe проверяет, что сервер отвечает: открывает соединение и делает
	// короткий обмен. Возвращает задержку.
	Probe(srv clientconf.Server, transport string, timeout time.Duration) (time.Duration, error)
}

type Pool struct {
	servers   []clientconf.Server
	transport string
	logf      func(string, ...any)
	probe     Dialer
	now       func() time.Time

	// пробы идут под мьютексом: пока один разбирается, остальные ждут и
	// получают уже выбранный сервер, а не устраивают свой перебор
	mu    sync.Mutex
	cur   int         // индекс текущего сервера, -1 пока не выбран
	until []time.Time // до этого момента сервер считаем нездоровым
}

// New создаёт пул. probe — как проверять сервер (в тестах подменяется).
func New(servers []clientconf.Server, transport string, logf func(string, ...any), probe Dialer) *Pool {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if probe == nil {
		probe = netProbe{}
	}
	return &Pool{
		servers:   append([]clientconf.Server(nil), servers...),
		transport: transport,
		logf:      logf,
		probe:     probe,
		now:       time.Now,
		cur:       -1,
		until:     make([]time.Time, len(servers)),
	}
}

// Current — сервер, на котором сидим сейчас. До первого Pick — пустой.
func (p *Pool) Current() clientconf.Server {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur < 0 {
		return clientconf.Server{}
	}
	return p.servers[p.cur]
}

// Pick выбирает сервер: идёт по списку сверху вниз и берёт первый живой.
// Возвращает ошибку, если не ответил ни один.
func (p *Pool) Pick(timeout time.Duration) (clientconf.Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pick(timeout)
}

// Failed сообщает, что текущий сервер не отвечает. Пул помечает его
// нездоровым на cooldown и выбирает следующий живой. Возвращает новый
// сервер или ошибку, если живых нет.
//
// Вызывающий не говорит, каким сервером пользовался, поэтому текущий сначала
// проверяем сами: если он отвечает, переключение уже прошло без нас и трогать
// ничего не нужно. Кто знает свой сервер — зовёт FailedOn, там пробы не будет.
func (p *Pool) Failed(err error, timeout time.Duration) (clientconf.Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur >= 0 {
		if _, e := p.probe.Probe(p.servers[p.cur], p.transport, timeout); e == nil {
			return p.servers[p.cur], nil
		}
	}
	return p.failed(err, timeout)
}

// FailedOn — то же, но вызывающий говорит, какой сервер отвалился. Если он уже
// не текущий, переключились без нас.
func (p *Pool) FailedOn(srv clientconf.Server, err error, timeout time.Duration) (clientconf.Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur >= 0 && p.servers[p.cur].Name != srv.Name {
		return p.servers[p.cur], nil
	}
	return p.failed(err, timeout)
}

// failed помечает текущий сервер нездоровым и ищет замену. Под мьютексом.
func (p *Pool) failed(err error, timeout time.Duration) (clientconf.Server, error) {
	if p.cur >= 0 {
		p.until[p.cur] = p.now().Add(cooldown)
		p.logf("сервер %s отвалился: %v", p.servers[p.cur].Name, err)
	}
	return p.pick(timeout)
}

// pick пробует кандидатов по порядку и оставляет текущим первого ответившего.
// Под мьютексом.
func (p *Pool) pick(timeout time.Duration) (clientconf.Server, error) {
	if len(p.servers) == 0 {
		return clientconf.Server{}, errors.New("sunfire: нет ни одного сервера")
	}
	var last error
	for _, i := range p.candidates() {
		srv := p.servers[i]
		rtt, err := p.probe.Probe(srv, p.transport, timeout)
		if err != nil {
			last = err
			p.until[i] = p.now().Add(cooldown)
			p.logf("сервер %s не отвечает: %v", srv.Name, err)
			continue
		}
		p.until[i] = time.Time{}
		switch {
		case p.cur < 0:
			p.logf("сервер %s (%s), задержка %v", srv.Name, srv.GW, rtt.Round(time.Millisecond))
		case p.cur != i:
			p.logf("переключаюсь на %s (%s), задержка %v", srv.Name, srv.GW, rtt.Round(time.Millisecond))
		}
		p.cur = i
		return srv, nil
	}
	if last == nil {
		last = errors.New("нечего пробовать")
	}
	return clientconf.Server{}, fmt.Errorf("sunfire: ни один сервер не отвечает: %w", last)
}

// candidates — индексы здоровых серверов в порядке списка. Если здоровых нет,
// берём всех: попытка лучше, чем сидеть без связи.
func (p *Pool) candidates() []int {
	now := p.now()
	live := make([]int, 0, len(p.servers))
	for i := range p.servers {
		if !p.until[i].After(now) {
			live = append(live, i)
		}
	}
	if len(live) == 0 {
		for i := range p.servers {
			live = append(live, i)
		}
	}
	return live
}

// netProbe — проба по-настоящему: подключиться и обменяться парой байт.
type netProbe struct{}

func (netProbe) Probe(srv clientconf.Server, tr string, timeout time.Duration) (time.Duration, error) {
	// без транспортного ключа udp не поднять, остаётся tcp
	names := transport.Available(srv.TransportKey)
	if tr != "" && tr != "auto" {
		if !slices.Contains(names, tr) {
			return 0, fmt.Errorf("sunfire: сервер %s: без transport_key доступен только tcp", srv.Name)
		}
		names = []string{tr}
	}
	var last error
	for _, name := range names {
		rtt, err := probeOnce(srv, name, timeout)
		if err == nil {
			return rtt, nil
		}
		last = err
	}
	return 0, last
}

func probeOnce(srv clientconf.Server, name string, timeout time.Duration) (time.Duration, error) {
	t, err := transport.Get(name)
	if err != nil {
		return 0, err
	}
	raw, err := t.Dial(srv.GW, srv.TransportKey, timeout)
	if err != nil {
		return 0, err
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(timeout))

	start := time.Now()
	c, err := proto.Dial(raw, srv.Token, probe.Target)
	if err != nil {
		return 0, err
	}
	buf := make([]byte, rttPayload)
	if _, err := c.Write(buf); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(c, buf); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}
