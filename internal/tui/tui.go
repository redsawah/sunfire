// Package tui — монитор состояния в терминале.
package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"sunfire/internal/stats"
)

const (
	tick      = time.Second
	graphRows = 5
	minWidth  = 60
)

var levels = []rune(" ▁▂▃▄▅▆▇█")

// Run опрашивает статус и рисует, пока не придёт сигнал.
func Run(addr string) error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h\033[0m\n")

	m := &meter{cpu: readCPU()}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		snap, err := fetch(addr)
		if err != nil {
			fmt.Print("\033[H\033[2J" + fmt.Sprintf("sunfire: нет ответа на %s\n%v\n", addr, err))
		} else {
			fmt.Print(m.frame(snap))
		}
		select {
		case <-stop:
			return nil
		case <-t.C:
		}
	}
}

func fetch(addr string) (stats.Snapshot, error) {
	var s stats.Snapshot
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/status")
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	return s, json.NewDecoder(resp.Body).Decode(&s)
}

// meter помнит прошлый снимок: скорости считаются по разнице.
type meter struct {
	prev    map[string][2]uint64
	prevAt  time.Time
	cpu     cpuSample
	histDn  []float64
	histUp  []float64
	lastCPU float64
}

func (m *meter) frame(s stats.Snapshot) string {
	w, h := size()
	now := time.Now()

	var dn, up float64
	cur := make(map[string][2]uint64, len(s.Users))
	rows := make([]row, 0, len(s.Users))
	var conns, total int64
	for _, u := range s.Users {
		cur[u.Name] = [2]uint64{u.Up, u.Down}
		var ru, rd float64
		if old, ok := m.prev[u.Name]; ok && !m.prevAt.IsZero() {
			if d := now.Sub(m.prevAt).Seconds(); d > 0.05 {
				if u.Up >= old[0] {
					ru = float64(u.Up-old[0]) * 8 / d
				}
				if u.Down >= old[1] {
					rd = float64(u.Down-old[1]) * 8 / d
				}
			}
		}
		up += ru
		dn += rd
		conns += u.Conns
		total += u.Total
		rows = append(rows, row{u, ru, rd})
	}
	m.prev, m.prevAt = cur, now

	gw := w
	if gw < minWidth {
		gw = minWidth
	}
	// высоту графиков и длину таблицы ужимаем так, чтобы журнал влез всегда
	grows, maxUsers := graphRows, 8
	switch {
	case h < 26:
		grows, maxUsers = 2, 3
	case h < 34:
		grows, maxUsers = 3, 5
	}
	m.histDn = push(m.histDn, dn, gw)
	m.histUp = push(m.histUp, up, gw)
	cpuNow, next := cpuPct(m.cpu)
	m.cpu = next
	if cpuNow > 0 {
		m.lastCPU = cpuNow
	}

	var b strings.Builder
	b.WriteString("\033[H\033[2J")

	// шапка
	b.WriteString(fmt.Sprintf("\033[1;32msunfire\033[0m \033[2m·\033[0m %s %s   \033[2mработает\033[0m %s\n",
		s.Role, s.Version, dur(time.Duration(s.Uptime)*time.Second)))
	if s.Extra != "" {
		b.WriteString("\033[2m" + cut(s.Extra, w) + "\033[0m\n")
	}
	b.WriteString(fmt.Sprintf("\033[1mсессий\033[0m %d \033[2m(всего %d)\033[0m   \033[36m↓ %s\033[0m   \033[35m↑ %s\033[0m\n\n",
		conns, total, bps(dn), bps(up)))

	// графики
	b.WriteString(graph("загрузка ↓", m.histDn, grows, "\033[36m"))
	b.WriteString(graph("отдача ↑", m.histUp, grows, "\033[35m"))

	// нагрузка
	b.WriteString(load(m.lastCPU, gw))

	// таблица
	b.WriteString("\n")
	b.WriteString(table(rows, w, maxUsers))

	// журнал — то, что осталось, но не меньше трёх строк
	used := strings.Count(b.String(), "\n")
	free := h - used - 3
	if free < 3 {
		free = 3
	}
	b.WriteString("\n\033[1mжурнал\033[0m\n")
	lines := s.Log
	if len(lines) > free {
		lines = lines[len(lines)-free:]
	}
	for _, l := range lines {
		b.WriteString("\033[2m" + cut(l, w) + "\033[0m\n")
	}
	b.WriteString("\033[2mCtrl+C — выход\033[0m")
	return b.String()
}

type row struct {
	u      stats.UserStat
	up, dn float64
}

func table(rows []row, w, max int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\033[1m%-24s %7s %8s %11s %11s %10s %10s\033[0m\n",
		"кто", "сессий", "всего", "↓", "↑", "принято", "отдано"))
	if len(rows) == 0 {
		b.WriteString("\033[2mнет активности\033[0m\n")
		return b.String()
	}
	for i, r := range rows {
		if i >= max {
			b.WriteString(fmt.Sprintf("\033[2m…ещё %d\033[0m\n", len(rows)-max))
			break
		}
		b.WriteString(fmt.Sprintf("%-24s %7d %8d %11s %11s %10s %10s\n",
			cut(r.u.Name, 24), r.u.Conns, r.u.Total, bps(r.dn), bps(r.up),
			size2(r.u.Down), size2(r.u.Up)))
	}
	return b.String()
}

// graph — столбики блочными символами.
func graph(title string, h []float64, rows int, color string) string {
	peak := 0.0
	for _, v := range h {
		if v > peak {
			peak = v
		}
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\033[2m%s, пик %s\033[0m\n", title, bps(peak)))
	if peak <= 0 {
		peak = 1
	}
	for r := rows - 1; r >= 0; r-- {
		lo := peak * float64(r) / float64(rows)
		hi := peak * float64(r+1) / float64(rows)
		b.WriteString(color)
		for _, v := range h {
			switch {
			case v >= hi:
				b.WriteRune(levels[8])
			case v <= lo:
				b.WriteRune(' ')
			default:
				i := int((v - lo) / (hi - lo) * 8)
				if i < 0 {
					i = 0
				}
				if i > 8 {
					i = 8
				}
				b.WriteRune(levels[i])
			}
		}
		b.WriteString("\033[0m\n")
	}
	return b.String()
}

func load(cpu float64, w int) string {
	la := loadAvg()
	mu, mt := mem()
	bw := w/2 - 22
	if bw < 8 {
		bw = 8
	}
	s := fmt.Sprintf("\033[1mCPU\033[0m %s %5.1f%%", bar(cpu/100, bw), cpu)
	if la[0] > 0 || la[1] > 0 {
		s += fmt.Sprintf("   \033[2mсредняя %.2f %.2f %.2f\033[0m", la[0], la[1], la[2])
	}
	s += "\n"
	if mt > 0 {
		s += fmt.Sprintf("\033[1mОЗУ\033[0m %s %s / %s\n", bar(float64(mu)/float64(mt), bw), size2(mu), size2(mt))
	}
	return s
}

func bar(frac float64, w int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	n := int(frac * float64(w))
	c := "\033[32m"
	switch {
	case frac > 0.85:
		c = "\033[31m"
	case frac > 0.6:
		c = "\033[33m"
	}
	return "[" + c + strings.Repeat("|", n) + "\033[0m" + strings.Repeat(" ", w-n) + "]"
}

func push(h []float64, v float64, w int) []float64 {
	h = append(h, v)
	if len(h) > w {
		h = h[len(h)-w:]
	}
	return h
}

// ─── система ────────────────────────────────────────────────────────────────

type cpuSample struct{ total, idle float64 }

func readCPU() cpuSample {
	d, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}
	}
	for _, l := range strings.Split(string(d), "\n") {
		if !strings.HasPrefix(l, "cpu ") {
			continue
		}
		var tot, idle float64
		for i, f := range strings.Fields(l)[1:] {
			v, err := strconv.ParseFloat(f, 64)
			if err != nil {
				continue
			}
			tot += v
			if i == 3 || i == 4 {
				idle += v
			}
		}
		return cpuSample{tot, idle}
	}
	return cpuSample{}
}

func cpuPct(prev cpuSample) (float64, cpuSample) {
	cur := readCPU()
	if prev.total == 0 || cur.total <= prev.total {
		return 0, cur
	}
	return (1 - (cur.idle-prev.idle)/(cur.total-prev.total)) * 100, cur
}

func loadAvg() [3]float64 {
	var la [3]float64
	d, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return la
	}
	f := strings.Fields(string(d))
	for i := 0; i < 3 && i < len(f); i++ {
		la[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return la
}

// mem возвращает использовано и всего, в байтах. Вне Linux — нули.
func mem() (uint64, uint64) {
	d, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	get := func(k string) uint64 {
		for _, l := range strings.Split(string(d), "\n") {
			if strings.HasPrefix(l, k) {
				f := strings.Fields(l)
				if len(f) >= 2 {
					v, _ := strconv.ParseUint(f[1], 10, 64)
					return v * 1024
				}
			}
		}
		return 0
	}
	tot, avail := get("MemTotal:"), get("MemAvailable:")
	if tot == 0 {
		return 0, 0
	}
	return tot - avail, tot
}

func size() (int, int) {
	var ws struct{ row, col, x, y uint16 }
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(),
		uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if e != 0 || ws.col == 0 {
		return 80, 24
	}
	return int(ws.col), int(ws.row)
}

// ─── форматирование ─────────────────────────────────────────────────────────

func bps(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.2f Гбит/с", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.1f Мбит/с", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.0f Кбит/с", v/1e3)
	default:
		return fmt.Sprintf("%.0f бит/с", v)
	}
}

func size2(v uint64) string {
	f := float64(v)
	switch {
	case f >= 1<<40:
		return fmt.Sprintf("%.1f ТБ", f/(1<<40))
	case f >= 1<<30:
		return fmt.Sprintf("%.1f ГБ", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.0f МБ", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.0f КБ", f/(1<<10))
	default:
		return fmt.Sprintf("%d Б", v)
	}
}

func dur(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dд%02dч", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dч%02dм", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dм%02dс", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dс", int(d.Seconds()))
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
