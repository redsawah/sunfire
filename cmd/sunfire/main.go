package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"sunfire/internal/clientconf"
	"sunfire/internal/failover"
	"sunfire/internal/menu"
	"sunfire/internal/probe"
	"sunfire/internal/proto"
	"sunfire/internal/selfupdate"
	"sunfire/internal/socks"
	"sunfire/internal/stats"
	"sunfire/internal/stripe"
	"sunfire/internal/transport"
	"sunfire/internal/tui"
	"sunfire/internal/version"
)

const (
	appName     = "sunfire"
	dialTimeout = 10 * time.Second

	// envPath — где настройки клиента лежали до появления client.conf.
	envPath = "/etc/sunfire/client.env"

	// quickProbe — когда транспорт задан, от пробы нужна только живость.
	quickProbe = 300 * time.Millisecond
)

func main() {
	cfgPath := flag.String("config", clientconf.DefaultPath, "путь к конфигу клиента")
	gw := flag.String("gw", "", "адрес шлюза host:port")
	token := flag.String("token", "", "токен")
	trKey := flag.String("transport-key", "", "транспортный ключ шлюза, вместе с -gw; без него доступен только tcp")
	socksAddr := flag.String("socks", clientconf.DefaultSocks, "адрес локального SOCKS5")
	tr := flag.String("transport", "auto", "транспорт: auto|tcp|udp")
	probeTime := flag.Duration("probe-time", 3*time.Second, "длительность замера одного транспорта")
	streams := flag.Int("streams", 1, "соединений на сессию")
	verbose := flag.Bool("v", false, "подробный лог")
	showVersion := flag.Bool("version", false, "напечатать версию и выйти")
	update := flag.Bool("update", false, "обновиться до последней версии и выйти")
	checkUpdate := flag.Bool("check-update", false, "проверить наличие новой версии и выйти")
	statusAddr := flag.String("status", clientconf.DefaultStatusAddr, "адрес статуса для монитора, только петля")
	top := flag.Bool("top", false, "открыть монитор и выйти")
	setup := flag.Bool("menu", false, "открыть меню настройки и выйти")
	flag.Parse()

	// какие флаги заданы явно: только они перекрывают конфиг
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	switch {
	case *showVersion:
		fmt.Println(version.Number)
		return
	case *update, *checkUpdate:
		if err := selfupdate.Run(appName, version.Number, *checkUpdate, log.Printf); err != nil {
			log.Fatal(err)
		}
		return
	case *setup:
		if err := menu.Client(*cfgPath); err != nil {
			log.Fatal(err)
		}
		return
	case *top:
		addr := *statusAddr
		if !set["status"] {
			if c, err := clientconf.Load(*cfgPath); err == nil {
				addr = c.StatusAddr
			}
		}
		if err := tui.Run(addr); err != nil {
			log.Fatal(err)
		}
		return
	}

	flags := clientconf.Config{Socks: *socksAddr, StatusAddr: *statusAddr, Transport: *tr, Streams: *streams}
	cfg, err := settings(*cfgPath, set, flags, clientconf.Server{GW: *gw, Token: *token, TransportKey: *trKey})
	if err != nil {
		log.Fatal(err)
	}
	if err := run(cfg, *probeTime, *verbose); err != nil {
		log.Fatal(err)
	}
}

// settings — откуда взять настройки. С -gw и -token работаем по флагам, как
// раньше; иначе по конфигу, а явно заданные флаги его перекрывают.
func settings(path string, set map[string]bool, flags clientconf.Config, srv clientconf.Server) (clientconf.Config, error) {
	srv.GW, srv.Token = strings.TrimSpace(srv.GW), strings.TrimSpace(srv.Token)
	srv.TransportKey = strings.TrimSpace(srv.TransportKey)
	if (srv.GW == "") != (srv.Token == "") {
		return clientconf.Config{}, errors.New("sunfire: нужны и -gw, и -token")
	}
	if srv.GW != "" {
		srv.Name = srv.GW
		flags.Servers = []clientconf.Server{srv}
		return flags, nil
	}

	cfg, err := load(path)
	if err != nil {
		return clientconf.Config{}, err
	}
	if set["socks"] {
		cfg.Socks = flags.Socks
	}
	if set["status"] {
		cfg.StatusAddr = flags.StatusAddr
	}
	if set["transport"] {
		cfg.Transport = flags.Transport
	}
	if set["streams"] {
		cfg.Streams = flags.Streams
	}
	return cfg, nil
}

// load читает конфиг клиента, а на старой установке — client.env.
func load(path string) (clientconf.Config, error) {
	cfg, err := clientconf.Load(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return cfg, err
	}
	env, envErr := clientconf.FromEnvFile(envPath)
	if envErr != nil {
		return clientconf.Config{}, err
	}
	log.Printf("конфига %s нет, работаю по старому %s — перенесите настройки: sunfire -menu", path, envPath)
	return env, nil
}

func run(cfg clientconf.Config, probeTime time.Duration, verbose bool) error {
	if err := requireLoopback("socks", cfg.Socks); err != nil {
		return err
	}
	if err := requireLoopback("status", cfg.StatusAddr); err != nil {
		return err
	}
	if cfg.Streams < 1 {
		cfg.Streams = 1
	}
	if len(cfg.Servers) == 0 {
		return errors.New("sunfire: нет ни одного сервера")
	}
	stats.HookLog(os.Stderr, 200)
	logf := func(format string, args ...any) {
		if verbose {
			log.Printf(format, args...)
		}
	}

	auto := cfg.Transport == "" || cfg.Transport == "auto"
	dur := quickProbe
	if auto {
		dur = probeTime
	}
	d := &gwDialer{
		probe:   &gwProbe{dur: dur},
		trName:  cfg.Transport,
		streams: cfg.Streams,
		total:   len(cfg.Servers),
		reg:     stats.New(),
	}

	// один сервер обходится без пула: проба нужна, только если выбираем транспорт
	if len(cfg.Servers) > 1 {
		d.pool = failover.New(cfg.Servers, cfg.Transport, log.Printf, d.probe)
		srv, err := d.pool.Pick(dialTimeout)
		if err != nil {
			return err
		}
		if err := d.use(srv); err != nil {
			return err
		}
	} else {
		srv := cfg.Servers[0]
		if auto {
			if _, err := d.probe.Probe(srv, cfg.Transport, dialTimeout); err != nil {
				return fmt.Errorf("sunfire: %s: %w", srv.GW, err)
			}
		}
		if err := d.use(srv); err != nil {
			return err
		}
	}

	ln, err := net.Listen("tcp", cfg.Socks)
	if err != nil {
		return fmt.Errorf("sunfire: слушать %s: %w", cfg.Socks, err)
	}
	defer ln.Close()
	serveStatus(cfg.StatusAddr, d.reg)

	l := d.active()
	log.Printf("SOCKS5 %s -> %s (%s)", cfg.Socks, l.srv.GW, l.name)
	if cfg.Streams > 1 {
		log.Printf("соединений на сессию: %d", cfg.Streams)
	}
	if len(cfg.Servers) > 1 {
		log.Printf("серверов в очереди: %d", len(cfg.Servers))
	}
	log.Printf("монитор: sunfire -top")

	srv := &socks.Server{Dialer: d, Logf: logf}
	return srv.Serve(ln)
}

// serveStatus поднимает статус для монитора. Только петля.
func serveStatus(addr string, reg *stats.Reg) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reg.Snapshot())
	})
	s := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := s.ListenAndServe(); err != nil {
			log.Printf("статус не поднялся: %v", err)
		}
	}()
}

// noKey — серверы, о нехватке транспортного ключа у которых уже сказали.
var noKey sync.Map

// trFor — какие транспорты пробовать на этом сервере: want из конфига либо все
// доступные. Без транспортного ключа udp не поднять, о чём говорим один раз.
func trFor(srv clientconf.Server, want string) []string {
	names := transport.Available(srv.TransportKey)
	auto := want == "" || want == "auto"
	if srv.TransportKey == "" && (auto || transport.NeedsKey(want)) {
		if _, said := noKey.LoadOrStore(srv.Name, true); !said {
			log.Printf("сервер %s: без transport_key доступен только tcp", srv.Name)
		}
	}
	if !auto && slices.Contains(names, want) {
		return []string{want}
	}
	return names
}

// gwProbe — проба сервера для пула. Заодно запоминает, какой транспорт на этом
// сервере быстрее: после переключения мерить ещё раз уже не нужно.
type gwProbe struct {
	dur time.Duration

	mu   sync.Mutex
	best map[string]string
}

func (p *gwProbe) Probe(srv clientconf.Server, tr string, timeout time.Duration) (time.Duration, error) {
	opts := probe.Options{
		Addr: srv.GW, Token: srv.Token, TransportKey: srv.TransportKey,
		Transports: trFor(srv, tr), Duration: p.dur, Timeout: timeout,
	}
	auto := tr == "" || tr == "auto"
	if auto {
		log.Printf("замер транспортов до %s…", srv.GW)
	}

	results := probe.Run(opts)
	if len(results) == 0 {
		return 0, errors.New("нечего мерить")
	}
	if auto {
		for _, r := range results {
			if r.Err != nil {
				log.Printf("  %-4s ошибка: %v", r.Transport, r.Err)
				continue
			}
			log.Printf("  %-4s %.0f Мбит/с, rtt %v", r.Transport, r.Mbps, r.RTT.Round(time.Millisecond))
		}
	}

	// живые в начале списка, самый быстрый первым
	if results[0].Err != nil {
		return 0, results[0].Err
	}
	p.mu.Lock()
	if p.best == nil {
		p.best = make(map[string]string)
	}
	p.best[srv.Name] = results[0].Transport
	p.mu.Unlock()
	return results[0].RTT, nil
}

// pick — что показал последний замер этого сервера.
func (p *gwProbe) pick(name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.best[name]
}

// link — сервер, на котором сидим сейчас, и транспорт до него.
type link struct {
	srv  clientconf.Server
	tr   transport.Transport
	name string
}

type gwDialer struct {
	pool    *failover.Pool // nil, когда сервер один
	probe   *gwProbe
	trName  string // из конфига: auto|tcp|udp
	streams int
	total   int
	reg     *stats.Reg

	mu  sync.RWMutex
	cur link
}

// use делает сервер активным и подбирает ему транспорт.
func (d *gwDialer) use(srv clientconf.Server) error {
	names := trFor(srv, d.trName)
	name := names[0]
	if d.trName == "" || d.trName == "auto" {
		if best := d.probe.pick(srv.Name); slices.Contains(names, best) {
			name = best
		}
	}
	t, err := transport.Get(name)
	if err != nil {
		return fmt.Errorf("sunfire: транспорт %s: %w", name, err)
	}
	d.mu.Lock()
	d.cur = link{srv: srv, tr: t, name: name}
	d.mu.Unlock()

	extra := srv.GW + " · " + name
	if d.total > 1 {
		extra = fmt.Sprintf("%s · %s · серверов: %d", srv.Name, extra, d.total)
	}
	if d.streams > 1 {
		extra += fmt.Sprintf(" · соединений на сессию: %d", d.streams)
	}
	d.reg.SetMeta("клиент", version.Number, extra)
	return nil
}

func (d *gwDialer) active() link {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cur
}

// counted считает трафик сессии; закрытие снимает её со счётчика.
type counted struct {
	io.ReadWriteCloser
	c *stats.Conn
}

func (c counted) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	c.c.Down(int64(n))
	return n, err
}

func (c counted) Write(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Write(p)
	c.c.Up(int64(n))
	return n, err
}

func (c counted) Close() error {
	c.c.Close()
	return c.ReadWriteCloser.Close()
}

// CloseWrite пробрасываем дальше: без него конец потока не доезжает до второй
// стороны и соединения висят до обрыва.
func (c counted) CloseWrite() error {
	if w, ok := c.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return nil
}

func (d *gwDialer) DialTarget(target string) (io.ReadWriteCloser, error) {
	l := d.active()
	rw, err := d.dialTarget(l, target)
	if err != nil && d.pool != nil {
		rw, err = d.next(l.srv, target, err)
	}
	if err != nil {
		return nil, err
	}
	host, _, e := net.SplitHostPort(target)
	if e != nil {
		host = target
	}
	return counted{rw, d.reg.Open(host)}, nil
}

// next переводит клиента на следующий сервер и пробует ещё раз — один раз:
// если и там глухо, переключением займётся следующая сессия.
func (d *gwDialer) next(bad clientconf.Server, target string, cause error) (io.ReadWriteCloser, error) {
	srv, err := d.pool.FailedOn(bad, cause, dialTimeout)
	if err != nil {
		return nil, err
	}
	if err := d.use(srv); err != nil {
		return nil, err
	}
	return d.dialTarget(d.active(), target)
}

func (d *gwDialer) dialTarget(l link, target string) (io.ReadWriteCloser, error) {
	if d.streams < 2 {
		return d.dial(l, target)
	}
	sid, err := stripe.NewSessionID()
	if err != nil {
		return nil, err
	}

	// соединения раскладки открываем разом: последовательно ушёл бы лишний RTT на каждое
	conns := make([]io.ReadWriteCloser, d.streams)
	errs := make([]error, d.streams)
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			head := stripe.FrameJoin(sid, i)
			if i == 0 {
				head = stripe.FrameStart(sid, d.streams, target)
			}
			conns[i], errs[i] = d.dial(l, head)
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			for _, c := range conns {
				if c != nil {
					c.Close()
				}
			}
			return nil, err
		}
	}
	return stripe.Join(conns), nil
}

func (d *gwDialer) dial(l link, head string) (io.ReadWriteCloser, error) {
	raw, err := l.tr.Dial(l.srv.GW, l.srv.TransportKey, dialTimeout)
	if err != nil {
		return nil, err
	}
	c, err := proto.Dial(raw, l.srv.Token, head)
	if err != nil {
		raw.Close()
		return nil, err
	}
	return halfConn{Conn: c, raw: raw}, nil
}

// halfConn доносит до шлюза конец своей половины потока, не закрывая обратную.
// Транспорт без половинного закрытия (udp) обходится без этого.
type halfConn struct {
	*proto.Conn
	raw net.Conn
}

func (c halfConn) CloseWrite() error {
	if w, ok := c.raw.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return nil
}

// requireLoopback — SOCKS и статус без авторизации, наружу их выставлять нельзя.
func requireLoopback(what, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("sunfire: %s: адрес вида 127.0.0.1:1080: %w", what, err)
	}
	ip := net.ParseIP(host)
	if strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("sunfire: %s: адрес обязан быть петлёй, а не %q", what, host)
}
