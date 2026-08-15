// Package stats — живые счётчики и кольцо лога.
package stats

import (
	"cmp"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// user — счётчики одного пользователя. Инкременты атомарные, без общего мьютекса.
type user struct {
	name  string
	conns atomic.Int64
	total atomic.Int64
	up    atomic.Uint64
	down  atomic.Uint64
}

// Reg — счётчики процесса.
type Reg struct {
	start time.Time

	mu    sync.RWMutex
	users map[string]*user
	role  string
	ver   string
	extra string
}

func New() *Reg {
	return &Reg{start: time.Now(), users: make(map[string]*user)}
}

func (r *Reg) SetMeta(role, version, extra string) {
	r.mu.Lock()
	r.role, r.ver, r.extra = role, version, extra
	r.mu.Unlock()
}

func (r *Reg) user(name string) *user {
	r.mu.RLock()
	u := r.users[name]
	r.mu.RUnlock()
	if u != nil {
		return u
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if u = r.users[name]; u == nil {
		u = &user{name: name}
		r.users[name] = u
	}
	return u
}

// Open регистрирует соединение пользователя. По завершении нужен Close.
func (r *Reg) Open(name string) *Conn {
	u := r.user(name)
	u.conns.Add(1)
	u.total.Add(1)
	return &Conn{u: u}
}

// Conn — дескриптор одного соединения.
type Conn struct {
	u    *user
	done atomic.Bool
}

// Up — байты клиент->цель.
func (c *Conn) Up(n int64) {
	if n > 0 {
		c.u.up.Add(uint64(n))
	}
}

// Down — байты цель->клиент.
func (c *Conn) Down(n int64) {
	if n > 0 {
		c.u.down.Add(uint64(n))
	}
}

// Close снимает соединение с учёта; повторный вызов безвреден.
func (c *Conn) Close() {
	if c.done.CompareAndSwap(false, true) {
		c.u.conns.Add(-1)
	}
}

type countReader struct {
	r   io.Reader
	add func(int64)
}

func (c countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.add(int64(n))
	}
	return n, err
}

type countWriter struct {
	w   io.Writer
	add func(int64)
}

func (c countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.add(int64(n))
	}
	return n, err
}

// CountReader считает прочитанные байты.
func CountReader(r io.Reader, add func(int64)) io.Reader { return countReader{r, add} }

// CountWriter считает записанные байты.
func CountWriter(w io.Writer, add func(int64)) io.Writer { return countWriter{w, add} }

type UserStat struct {
	Name  string `json:"name"`
	Conns int64  `json:"conns"`
	Total int64  `json:"total"`
	Up    uint64 `json:"up"`
	Down  uint64 `json:"down"`
}

// Snapshot — то, что отдаётся монитору.
type Snapshot struct {
	Uptime  int64      `json:"uptime"`
	Version string     `json:"version"`
	Role    string     `json:"role"`
	Extra   string     `json:"extra"`
	Users   []UserStat `json:"users"`
	Log     []string   `json:"log"`
}

// Snapshot сортирует пользователей по трафику по убыванию.
func (r *Reg) Snapshot() Snapshot {
	s := Snapshot{
		Uptime: int64(time.Since(r.start).Seconds()),
		Log:    LogLines(),
	}
	r.mu.RLock()
	s.Role, s.Version, s.Extra = r.role, r.ver, r.extra
	s.Users = make([]UserStat, 0, len(r.users))
	for _, u := range r.users {
		s.Users = append(s.Users, UserStat{
			Name:  u.name,
			Conns: u.conns.Load(),
			Total: u.total.Load(),
			Up:    u.up.Load(),
			Down:  u.down.Load(),
		})
	}
	r.mu.RUnlock()

	slices.SortFunc(s.Users, func(a, b UserStat) int {
		if d := cmp.Compare(b.Up+b.Down, a.Up+a.Down); d != 0 {
			return d
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return s
}

var ring struct {
	mu    sync.Mutex
	lines []string
	pos   int
	full  bool
	out   io.Writer
}

type hook struct{}

// HookLog подменяет вывод log: пишет в w и хранит последние n строк.
func HookLog(w io.Writer, n int) {
	if n < 1 {
		n = 1
	}
	ring.mu.Lock()
	ring.lines = make([]string, n)
	ring.pos, ring.full, ring.out = 0, false, w
	ring.mu.Unlock()
	log.SetOutput(hook{})
}

func (hook) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	ring.mu.Lock()
	out := ring.out
	for s := range strings.SplitSeq(strings.TrimRight(string(p), "\n"), "\n") {
		ring.lines[ring.pos] = s
		ring.pos++
		if ring.pos == len(ring.lines) {
			ring.pos = 0
			ring.full = true
		}
	}
	ring.mu.Unlock()

	if out != nil {
		return out.Write(p)
	}
	return len(p), nil
}

// LogLines — последние строки лога, от старых к новым, без перевода строки.
func LogLines() []string {
	ring.mu.Lock()
	defer ring.mu.Unlock()
	out := make([]string, 0, len(ring.lines))
	if ring.full {
		out = append(out, ring.lines[ring.pos:]...)
	}
	return append(out, ring.lines[:ring.pos]...)
}
