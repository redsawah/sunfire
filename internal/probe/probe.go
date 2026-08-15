// Package probe — замер скорости транспортов до шлюза.
package probe

import (
	"errors"
	"io"
	"sort"
	"time"

	"sunfire/internal/proto"
	"sunfire/internal/transport"
)

// Target — псевдоадрес: шлюз ничего не набирает, а отвечает на первый кадр и глотает остальное.
const Target = "@probe"

const (
	defaultDuration = 3 * time.Second
	defaultTimeout  = 10 * time.Second
	rttPayload      = 32
)

type Result struct {
	Transport string
	Mbps      float64
	RTT       time.Duration
	Err       error
}

type Options struct {
	Addr       string            // адрес шлюза (host:port по умолчанию для каждого транспорта)
	Addrs      map[string]string // необязательно: транспорт -> свой адрес, перекрывает Addr
	Token      string
	Transports []string      // если пусто — transport.Names
	Duration   time.Duration // сколько лить, по умолчанию 3s
	Timeout    time.Duration // таймаут подключения, по умолчанию 10s
	Logf       func(string, ...any)
}

func (o Options) addrFor(name string) string {
	if a := o.Addrs[name]; a != "" {
		return a
	}
	return o.Addr
}

// Run измеряет все транспорты последовательно и возвращает результаты,
// отсортированные по убыванию скорости (ошибочные — в конце).
func Run(opts Options) []Result {
	names := opts.Transports
	if len(names) == 0 {
		names = transport.Names
	}
	if opts.Duration <= 0 {
		opts.Duration = defaultDuration
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	results := make([]Result, 0, len(names))
	for _, name := range names {
		r := measure(name, opts)
		if r.Err != nil {
			logf("проба %s: %v", name, r.Err)
		} else {
			logf("проба %s: %.1f Мбит/с, rtt %v", name, r.Mbps, r.RTT.Round(time.Millisecond))
		}
		results = append(results, r)
	}

	sort.Slice(results, func(i, j int) bool {
		if (results[i].Err == nil) != (results[j].Err == nil) {
			return results[i].Err == nil
		}
		return results[i].Mbps > results[j].Mbps
	})
	return results
}

// Best возвращает имя самого быстрого живого транспорта.
func Best(results []Result) (string, error) {
	best := ""
	top := -1.0
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		if r.Mbps > top {
			top, best = r.Mbps, r.Transport
		}
	}
	if best == "" {
		return "", errors.New("sunfire: ни один транспорт не отозвался")
	}
	return best, nil
}

func measure(name string, opts Options) Result {
	t, err := transport.Get(name)
	if err != nil {
		return Result{Transport: name, Err: err}
	}
	addr := opts.addrFor(name)
	if addr == "" {
		return Result{Transport: name, Err: errors.New("sunfire: не задан адрес шлюза")}
	}

	raw, err := t.Dial(addr, opts.Token, opts.Timeout)
	if err != nil {
		return Result{Transport: name, Err: err}
	}
	defer raw.Close() // proto.Conn.Close закрывает то же самое

	raw.SetDeadline(time.Now().Add(opts.Timeout))
	c, err := proto.Dial(raw, opts.Token, Target)
	if err != nil {
		return Result{Transport: name, Err: err}
	}

	rtt, err := roundTrip(c)
	if err != nil {
		return Result{Transport: name, Err: err}
	}

	raw.SetDeadline(time.Now().Add(opts.Duration + opts.Timeout))
	mbps, err := throughput(c, opts.Duration)
	if err != nil {
		return Result{Transport: name, Err: err}
	}
	return Result{Transport: name, Mbps: mbps, RTT: rtt}
}

func roundTrip(c *proto.Conn) (time.Duration, error) {
	out := make([]byte, rttPayload)
	start := time.Now()
	if _, err := c.Write(out); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(c, make([]byte, rttPayload)); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

func throughput(c *proto.Conn, d time.Duration) (float64, error) {
	buf := make([]byte, proto.MaxPayload)
	var total int64
	start := time.Now()
	for time.Since(start) < d {
		n, err := c.Write(buf)
		total += int64(n)
		if err != nil {
			return 0, err
		}
	}
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return 0, errors.New("sunfire: нулевая длительность пробы")
	}
	return float64(total) * 8 / elapsed / 1e6, nil
}
