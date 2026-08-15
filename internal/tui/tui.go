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
	prev     map[string][2]uint64
	prevAt   time.Time
	cpu      cpuSample
	cores    []cpuSample
	lastCore []float64
	histDn   []float64
	histUp   []float64
	lastCPU  float64
}

func (m *meter) frame(s stats.Snapshot) string {
	w, h := size()
	now := time.Now()

	var dn, up float64
	cur := make(map[string][2]uint64, len(s.Users))
	rows := make([]row, 0, len(s.Users))
	var conns, total int64
	var sumUp, sumDn uint64
	for _, u := range s.Users {
		sumUp += u.Up
		sumDn += u.Down
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

	inner := w - 2 // ширина внутри рамки
	if inner < minWidth {
		inner = minWidth
	}
	m.histDn = push(m.histDn, dn, inner)
	m.histUp = push(m.histUp, up, inner)
	cpuNow, next := cpuPct(m.cpu)
	m.cpu = next
	if cpuNow > 0 {
		m.lastCPU = cpuNow
	}
	if cl := coreLoad(m.cores); len(cl) > 0 {
		m.lastCore = cl
	}
	m.cores = readCores()

	// каждая панель — две строки рамки плюс содержимое
	sys := sysLines(m, inner)
	fixed := 5*2 /* рамки */ + 2 /* шапка */ + len(sys) + 1 /* заголовок таблицы */ + 1 /* низ */
	free := h - fixed
	if free < 8 {
		free = 8
	}
	grows := free / 4
	if grows > 12 {
		grows = 12
	}
	if grows < 2 {
		grows = 2
	}
	rest := free - 2*grows
	maxUsers := len(rows)
	if maxUsers > rest/2 {
		maxUsers = rest / 2
	}
	if maxUsers < 1 {
		maxUsers = 1
	}
	logRows := rest - maxUsers
	if logRows < 3 {
		logRows = 3
	}

	var b strings.Builder
	b.WriteString("\033[H\033[2J")

	head := []string{}
	if s.Extra != "" {
		head = append(head, " \033[2m"+cut(s.Extra, inner-2)+"\033[0m")
	}
	hl := fmt.Sprintf(" \033[1mсессий\033[0m %d \033[2m(всего %d)\033[0m   \033[36m↓ %s\033[0m   \033[35m↑ %s\033[0m",
		conns, total, bps(dn), bps(up))
	if inner >= 80 {
		hl += fmt.Sprintf("   \033[2mпринято\033[0m %s \033[2mотдано\033[0m %s", size2(sumDn), size2(sumUp))
	}
	head = append(head, hl)
	b.WriteString(panel(fmt.Sprintf("sunfire · %s %s · работает %s", s.Role, s.Version,
		dur(time.Duration(s.Uptime)*time.Second)), head, w))

	b.WriteString(panel("загрузка ↓  пик "+bps(peakOf(m.histDn)),
		graphLines(m.histDn, grows, inner, "\033[36m"), w))
	b.WriteString(panel("отдача ↑  пик "+bps(peakOf(m.histUp)),
		graphLines(m.histUp, grows, inner, "\033[35m"), w))
	b.WriteString(panel("система", sys, w))
	b.WriteString(panel("сессии", tableLines(rows, inner, maxUsers), w))

	lines := s.Log
	if len(lines) > logRows {
		lines = lines[len(lines)-logRows:]
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, " \033[2m"+cut(l, inner-2)+"\033[0m")
	}
	b.WriteString(panel("журнал", out, w))
	b.WriteString("\033[2mCtrl+C — выход\033[0m")
	return b.String()
}

func peakOf(h []float64) float64 {
	p := 0.0
	for _, v := range h {
		if v > p {
			p = v
		}
	}
	return p
}

// sysLines — нагрузка машины строками для панели.
func sysLines(m *meter, inner int) []string {
	la := loadAvg()
	mu, mt := mem()
	bw := inner/3 - 8
	if bw < 10 {
		bw = 10
	}
	out := []string{}
	s := fmt.Sprintf(" \033[1mCPU\033[0m %s %5.1f%%", bar(m.lastCPU/100, bw), m.lastCPU)
	if la[0] > 0 || la[1] > 0 {
		s += fmt.Sprintf("   \033[2mсредняя %.2f %.2f %.2f\033[0m", la[0], la[1], la[2])
	}
	out = append(out, s)
	if mt > 0 {
		out = append(out, fmt.Sprintf(" \033[1mОЗУ\033[0m %s %s / %s",
			bar(float64(mu)/float64(mt), bw), size2(mu), size2(mt)))
	}
	if len(m.lastCore) > 0 {
		out = append(out, " "+coreBar(m.lastCore, inner))
	}
	return out
}

// visLen — видимая длина строки: escape-последовательности не считаются.
func visLen(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			if r == 'm' {
				esc = false
			}
		case r == '\033':
			esc = true
		default:
			n++
		}
	}
	return n
}

// panel рисует блок в рамке с заголовком.
func panel(title string, lines []string, w int) string {
	if w < 20 {
		w = 20
	}
	inner := w - 2
	var b strings.Builder
	t := cut(title, inner-4)
	b.WriteString("\033[2m┌─\033[0m \033[1m" + t + "\033[0m \033[2m")
	dash := inner - visLen(t) - 3
	if dash < 0 {
		dash = 0
	}
	b.WriteString(strings.Repeat("─", dash) + "┐\033[0m\n")
	for _, l := range lines {
		pad := inner - visLen(l)
		if pad < 0 {
			l = clipVis(l, inner)
			pad = 0
		}
		b.WriteString("\033[2m│\033[0m" + l + strings.Repeat(" ", pad) + "\033[2m│\033[0m\n")
	}
	b.WriteString("\033[2m└" + strings.Repeat("─", inner) + "┘\033[0m\n")
	return b.String()
}

// clipVis обрезает по видимой длине, не ломая escape-последовательности.
func clipVis(s string, n int) string {
	var b strings.Builder
	cnt, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			b.WriteRune(r)
			if r == 'm' {
				esc = false
			}
		case r == '\033':
			esc = true
			b.WriteRune(r)
		default:
			if cnt >= n {
				continue
			}
			b.WriteRune(r)
			cnt++
		}
	}
	return b.String()
}

// coreBar — по столбику на ядро.
func coreBar(cl []float64, w int) string {
	var b strings.Builder
	b.WriteString("\033[1mядра\033[0m ")
	for _, v := range cl {
		i := int(v / 100 * 8)
		if i < 1 {
			i = 1 // простаивающее ядро всё равно видно
		}
		if i > 8 {
			i = 8
		}
		c := "\033[32m"
		switch {
		case v > 85:
			c = "\033[31m"
		case v > 60:
			c = "\033[33m"
		}
		b.WriteString(c + string(levels[i]) + "\033[0m")
	}
	return b.String()
}

type row struct {
	u      stats.UserStat
	up, dn float64
}

// tableLines подгоняет колонки под ширину: на узком экране объёмы и «всего» уходят.
func tableLines(rows []row, inner, max int) []string {
	vol := inner >= 92 // показывать принято/отдано
	tot := inner >= 74 // показывать «всего»
	name := inner - 1 - 7 - 13 - 13 - 4
	if tot {
		name -= 9
	}
	if vol {
		name -= 22
	}
	if name < 10 {
		name = 10
	}

	line := func(n string, conns, total any, dn, up, rx, tx string) string {
		s := fmt.Sprintf(" %-*s %7v", name, cut(n, name), conns)
		if tot {
			s += fmt.Sprintf(" %8v", total)
		}
		s += fmt.Sprintf(" %12s %12s", dn, up)
		if vol {
			s += fmt.Sprintf(" %10s %10s", rx, tx)
		}
		return s
	}

	out := []string{"\033[1m" + line("кто", "сессий", "всего", "↓", "↑", "принято", "отдано") + "\033[0m"}
	if len(rows) == 0 {
		return append(out, " \033[2mнет активности\033[0m")
	}
	for i, r := range rows {
		if i >= max {
			out = append(out, fmt.Sprintf(" \033[2m…ещё %d\033[0m", len(rows)-max))
			break
		}
		out = append(out, line(r.u.Name, r.u.Conns, r.u.Total,
			bps(r.dn), bps(r.up), size2(r.u.Down), size2(r.u.Up)))
	}
	return out
}

// graphLines — столбики блочными символами. Нижняя строка всегда рисуется,
// даже при нуле: так видно, что время идёт, а график не замер.
func graphLines(h []float64, rows, inner int, color string) []string {
	peak := peakOf(h)
	if peak <= 0 {
		peak = 1
	}
	// график прижат вправо: свежее — у правого края, старое уползает влево
	pad := inner - 1 - len(h)
	if pad < 0 {
		pad = 0
	}
	out := make([]string, 0, rows)
	for r := rows - 1; r >= 0; r-- {
		lo := peak * float64(r) / float64(rows)
		hi := peak * float64(r+1) / float64(rows)
		var b strings.Builder
		b.WriteString(" ")
		if r == 0 {
			b.WriteString("\033[2m" + strings.Repeat("▁", pad) + "\033[0m")
		} else {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString(color)
		for _, v := range h {
			switch {
			case v >= hi:
				b.WriteRune(levels[8])
			case v <= lo:
				if r == 0 {
					b.WriteString("\033[2m▁" + color) // базовая линия
				} else {
					b.WriteRune(' ')
				}
			default:
				i := int((v - lo) / (hi - lo) * 8)
				if i < 1 {
					i = 1
				}
				if i > 8 {
					i = 8
				}
				b.WriteRune(levels[i])
			}
		}
		b.WriteString("\033[0m")
		out = append(out, b.String())
	}
	return out
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

// readCores — сэмплы по каждому ядру.
func readCores() []cpuSample {
	d, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil
	}
	var out []cpuSample
	for _, l := range strings.Split(string(d), "\n") {
		if !strings.HasPrefix(l, "cpu") || strings.HasPrefix(l, "cpu ") {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 6 {
			continue
		}
		var tot, idle float64
		for i, v := range f[1:] {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			tot += n
			if i == 3 || i == 4 {
				idle += n
			}
		}
		out = append(out, cpuSample{tot, idle})
	}
	return out
}

// coreLoad — загрузка каждого ядра по разнице с прошлым сэмплом.
func coreLoad(prev []cpuSample) []float64 {
	cur := readCores()
	if len(cur) == 0 {
		return nil
	}
	out := make([]float64, len(cur))
	for i := range cur {
		if i < len(prev) && prev[i].total > 0 && cur[i].total > prev[i].total {
			out[i] = (1 - (cur[i].idle-prev[i].idle)/(cur[i].total-prev[i].total)) * 100
		}
	}
	return out
}

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
