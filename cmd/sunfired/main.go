package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"sunfire/internal/config"
	"sunfire/internal/probe"
	"sunfire/internal/proto"
	"sunfire/internal/selfupdate"
	"sunfire/internal/stats"
	"sunfire/internal/stripe"
	"sunfire/internal/transport"
	"sunfire/internal/tui"
	"sunfire/internal/version"
)

const (
	appName          = "sunfired"
	handshakeTimeout = 15 * time.Second
	dialTimeout      = 20 * time.Second
	// по размеру кадра: Read всё равно не отдаёт больше одного кадра за вызов
	bufSize        = proto.MaxPayload
	sessionTimeout = 15 * time.Second
	maxStripe      = 16
)

func main() {
	cfgPath := flag.String("config", "/etc/sunfire/main.conf", "путь к конфигу")
	newToken := flag.Bool("token", false, "напечатать новый токен и выйти")
	showVersion := flag.Bool("version", false, "напечатать версию и выйти")
	update := flag.Bool("update", false, "обновиться до последней версии и выйти")
	checkUpdate := flag.Bool("check-update", false, "проверить наличие новой версии и выйти")
	top := flag.Bool("top", false, "открыть монитор и выйти")
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println(version.Number)
		return
	case *update, *checkUpdate:
		if err := selfupdate.Run(appName, version.Number, *checkUpdate, log.Printf); err != nil {
			log.Fatal(err)
		}
		return
	case *top:
		addr := config.DefaultStatusAddr
		if c, err := config.Load(*cfgPath); err == nil && c.StatusAddr != "" {
			addr = c.StatusAddr
		}
		if err := tui.Run(addr); err != nil {
			log.Fatal(err)
		}
		return
	}

	if *newToken {
		t, err := config.NewToken()
		if err != nil {
			log.Fatalf("sunfired: %v", err)
		}
		fmt.Println(t)
		return
	}

	if err := run(*cfgPath); err != nil {
		log.Fatalf("sunfired: %v", err)
	}
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	tokens := cfg.Tokens()
	stats.HookLog(os.Stderr, 200)
	reg = stats.New()

	var wg sync.WaitGroup
	errc := make(chan error, len(cfg.Listen))
	where := make([]string, 0, len(cfg.Listen))
	for name, addr := range cfg.Listen {
		t, err := transport.Get(name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		ln, err := t.Listen(addr, firstToken(tokens))
		if err != nil {
			return fmt.Errorf("слушать %s %s: %w", name, addr, err)
		}
		defer ln.Close()
		log.Printf("%s %s", name, addr)
		where = append(where, name+" "+addr)

		wg.Add(1)
		go func(name string, ln net.Listener) {
			defer wg.Done()
			errc <- serve(ln, cfg, tokens)
		}(name, ln)
	}
	if len(cfg.Listen) == 0 {
		return errors.New("нечего слушать")
	}
	sort.Strings(where)
	reg.SetMeta("сервер", version.Number, strings.Join(where, " · ")+fmt.Sprintf(" · пользователей: %d", len(tokens)))
	serveStatus(cfg.StatusAddr, reg)
	log.Printf("монитор: sunfired -top")

	err = <-errc
	wg.Wait()
	return err
}

var reg *stats.Reg

// serveStatus поднимает статус для монитора. Только петля.
func serveStatus(addr string, r *stats.Reg) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.Snapshot())
	})
	s := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := s.ListenAndServe(); err != nil {
			log.Printf("статус не поднялся: %v", err)
		}
	}()
}

// firstToken — ключ транспортного слоя. Все клиенты этого шлюза используют
// общий транспортный ключ, а различаются токеном внутри потока.
func firstToken(tokens map[string]string) string {
	for t := range tokens {
		return t
	}
	return ""
}

func serve(ln net.Listener, cfg config.Config, tokens map[string]string) error {
	for {
		raw, err := ln.Accept()
		if err != nil {
			return err
		}
		go handle(raw, cfg, tokens)
	}
}

func handle(raw net.Conn, cfg config.Config, tokens map[string]string) {
	// соединение раскладки живёт дальше в сессии, закрывать его тут нельзя
	keep := false
	defer func() {
		if !keep {
			raw.Close()
		}
	}()

	_ = raw.SetDeadline(time.Now().Add(handshakeTimeout))
	conn, first, user, err := proto.Accept(raw, tokens)
	if err != nil {
		return
	}
	_ = raw.SetDeadline(time.Time{})

	ip := remoteIP(raw)
	if !cfg.Allowed(user, ip) {
		log.Printf("отказ %s: адрес не разрешён", user)
		return
	}
	if cfg.PinClientIP && !pins.check(user, ip) {
		log.Printf("отказ %s: токен закреплён за другим адресом", user)
		return
	}

	head, err := stripe.ParseHead(first)
	if err != nil {
		return
	}

	switch head.Kind {
	case stripe.KindStart:
		if head.Count > maxStripe {
			return
		}
		conns, ok := sess.start(head.SID, user, head.Target, head.Count, conn)
		if !ok {
			return
		}
		keep = true
		if conns != nil {
			serveStripe(conns, head.Target, user)
		}

	case stripe.KindJoin:
		conns, target, ok := sess.join(head.SID, user, head.Index, conn)
		if !ok {
			return
		}
		keep = true
		if conns != nil {
			serveStripe(conns, target, user)
		}

	default:
		serveOne(halfConn{Conn: conn, raw: raw}, head.Target, user)
	}
}

// halfConn доносит до клиента конец своей половины потока, не закрывая обратную.
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

func serveOne(conn io.ReadWriteCloser, target, user string) {
	if target == probe.Target {
		sink(conn)
		return
	}
	out, err := net.DialTimeout("tcp", target, dialTimeout)
	if err != nil {
		return
	}
	defer out.Close()
	c := reg.Open(user)
	defer c.Close()
	// Up — от клиента к цели, Down — обратно
	splice(conn, countConn{out, c})
}

// countConn считает трафик сессии на стороне цели.
type countConn struct {
	net.Conn
	c *stats.Conn
}

func (c countConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.c.Down(int64(n))
	return n, err
}

func (c countConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.c.Up(int64(n))
	return n, err
}

// CloseWrite пробрасываем дальше: без него конец потока не доезжает до цели.
func (c countConn) CloseWrite() error {
	if w, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return nil
}

// serveStripe склеивает собранную раскладку и обслуживает её как одну сессию.
func serveStripe(conns []io.ReadWriteCloser, target, user string) {
	s := stripe.Join(conns)
	defer s.Close()
	serveOne(s, target, user)
}

// sink обслуживает замер: отражает первую посылку и дальше поглощает поток.
func sink(conn io.ReadWriteCloser) {
	buf := make([]byte, bufSize)
	n, err := conn.Read(buf)
	if err != nil {
		return
	}
	if _, err := conn.Write(buf[:n]); err != nil {
		return
	}
	_, _ = io.CopyBuffer(io.Discard, conn, buf)
}

func splice(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(b, a, &wg)
	go pipe(a, b, &wg)
	wg.Wait()
}

// буферы сплайса переиспользуются: при десятках тысяч соединений своя пара на
// каждое съедала бы память быстрее всего остального
var bufPool = sync.Pool{New: func() any { b := make([]byte, bufSize); return &b }}

func pipe(dst, src io.ReadWriteCloser, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := bufPool.Get().(*[]byte)
	_, _ = io.CopyBuffer(dst, src, *buf)
	bufPool.Put(buf)
	// половинное закрытие: конец потока едет дальше, а обратное направление живёт
	if w, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = w.CloseWrite()
	}
}

// pending — раскладка, ждущая свои соединения.
type pending struct {
	user   string
	target string
	conns  []io.ReadWriteCloser
	n      int // объявленный размер; 0 — пока не пришло @s
	got    int
	timer  *time.Timer
}

type sessions struct {
	mu sync.Mutex
	m  map[string]*pending
}

var sess = &sessions{m: map[string]*pending{}}

// at находит или заводит сессию: соединения открываются разом и приходят
// в любом порядке, поэтому @j может опередить @s. Чужой sid — nil.
func (s *sessions) at(sid, user string) *pending {
	p := s.m[sid]
	if p == nil {
		p = &pending{user: user, conns: make([]io.ReadWriteCloser, maxStripe)}
		p.timer = time.AfterFunc(sessionTimeout, func() { s.drop(sid, p) })
		s.m[sid] = p
		return p
	}
	if p.user != user {
		return nil
	}
	return p
}

// start кладёт нулевое соединение и объявляет размер раскладки.
func (s *sessions) start(sid, user, target string, n int, c io.ReadWriteCloser) ([]io.ReadWriteCloser, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.at(sid, user)
	if p == nil || p.n != 0 || p.conns[0] != nil {
		return nil, false
	}
	for _, late := range p.conns[n:] {
		if late != nil { // уже пришёл индекс за пределами объявленного размера
			return nil, false
		}
	}
	p.target, p.n, p.conns[0] = target, n, c
	p.got++
	return s.ready(sid, p), true
}

// join кладёт соединение по индексу; собранную раскладку отдаёт наружу.
func (s *sessions) join(sid, user string, idx int, c io.ReadWriteCloser) ([]io.ReadWriteCloser, string, bool) {
	if idx >= maxStripe {
		return nil, "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.at(sid, user)
	if p == nil || p.conns[idx] != nil || (p.n != 0 && idx >= p.n) {
		return nil, "", false
	}
	p.conns[idx] = c
	p.got++
	return s.ready(sid, p), p.target, true
}

// ready отдаёт раскладку, когда собраны все объявленные соединения.
func (s *sessions) ready(sid string, p *pending) []io.ReadWriteCloser {
	if p.n == 0 || p.got < p.n {
		return nil
	}
	delete(s.m, sid)
	p.timer.Stop()
	return p.conns[:p.n]
}

// drop выбрасывает несобравшуюся сессию вместе с её соединениями.
func (s *sessions) drop(sid string, p *pending) {
	s.mu.Lock()
	if s.m[sid] != p {
		s.mu.Unlock()
		return
	}
	delete(s.m, sid)
	s.mu.Unlock()
	for _, c := range p.conns {
		if c != nil {
			c.Close()
		}
	}
	log.Printf("раскладка %s не собралась", sid)
}

// pinned — какой адрес закреплён за токеном. Первый успешно подключившийся адрес
// запоминается, остальные отбиваются. Живёт в памяти: рестарт снимает привязку.
type pinned struct {
	mu sync.Mutex
	ip map[string]string
}

var pins = &pinned{ip: map[string]string{}}

func (p *pinned) check(user string, ip net.IP) bool {
	if ip == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	got, ok := p.ip[user]
	if !ok {
		p.ip[user] = ip.String()
		log.Printf("%s закреплён за %s", user, ip)
		return true
	}
	return got == ip.String()
}

func remoteIP(c net.Conn) net.IP {
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}
