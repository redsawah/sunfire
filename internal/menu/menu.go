// Package menu — интерактивная настройка в терминале.
package menu

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"sunfire/internal/clientconf"
	"sunfire/internal/config"
	"sunfire/internal/tui"
	"sunfire/internal/version"
)

const (
	clearScr    = "\033[H\033[2J"
	unitServer  = "sunfired"
	unitClient  = "sunfire"
	minTokenLen = 16
	maxStreams  = 16

	// envPath — настройки клиента до появления client.conf.
	envPath = "/etc/sunfire/client.env"
)

// Server — меню сервера. cfgPath — путь к main.conf.
func Server(cfgPath string) error {
	m := &serverMenu{ui: newUI(), path: cfgPath}
	cfg, err := config.Load(cfgPath)
	switch {
	case err == nil:
		m.cfg = cfg
	case errors.Is(err, fs.ErrNotExist):
		m.printf("конфига %s нет\n", cfgPath)
		yes, ok := m.confirm("создать с умолчаниями?")
		if !ok || !yes {
			return nil
		}
		m.cfg, m.dirty = defaultServer(), true
	default:
		return err
	}
	m.run()
	return nil
}

// Client — меню клиента. cfgPath — путь к client.conf.
func Client(cfgPath string) error {
	m := &clientMenu{ui: newUI(), path: cfgPath}
	cfg, err := clientconf.Load(cfgPath)
	switch {
	case err == nil:
		m.cfg = cfg
	case errors.Is(err, fs.ErrNotExist):
		if !m.create(envPath) {
			return nil
		}
	default:
		return err
	}
	m.run()
	return nil
}

// create — конфига нет: перенести настройки со старой установки или начать с умолчаний.
func (m *clientMenu) create(envPath string) bool {
	m.printf("конфига %s нет\n", m.path)
	if env, err := clientconf.FromEnvFile(envPath); err == nil {
		yes, ok := m.confirm("перенести настройки из " + envPath + "?")
		if !ok {
			return false
		}
		if yes {
			m.cfg, m.dirty = env, true
			return true
		}
	}
	yes, ok := m.confirm("создать с умолчаниями?")
	if !ok || !yes {
		return false
	}
	m.cfg, m.dirty = clientconf.Default(), true
	return true
}

// ─── ввод и вывод ───────────────────────────────────────────────────────────

// ui — построчный ввод: сырой режим терминала не нужен, так меню одинаково
// работает и локально, и по ssh.
type ui struct {
	in  *bufio.Scanner
	out io.Writer
}

func newUI() ui { return ui{in: bufio.NewScanner(os.Stdin), out: os.Stdout} }

func (u ui) print(s string)                 { fmt.Fprint(u.out, s) }
func (u ui) printf(format string, a ...any) { fmt.Fprintf(u.out, format, a...) }

// ask читает строку. Второе значение — false на конце ввода (Ctrl+D).
func (u ui) ask(prompt string) (string, bool) {
	u.print(prompt)
	if !u.in.Scan() {
		u.print("\n")
		return "", false
	}
	return strings.TrimSpace(u.in.Text()), true
}

func (u ui) pause() bool {
	_, ok := u.ask("\nEnter — дальше ")
	return ok
}

// fail показывает ошибку и ждёт: экран перерисуется, и её надо успеть прочесть.
func (u ui) fail(err error) bool {
	u.printf("%v\n", hint(err))
	return u.pause()
}

// confirm — вопрос да/нет, по умолчанию нет.
func (u ui) confirm(q string) (bool, bool) {
	for {
		s, ok := u.ask(q + " [д/Н]: ")
		if !ok {
			return false, false
		}
		switch strings.ToLower(s) {
		case "д", "да", "y", "yes":
			return true, true
		case "", "н", "нет", "n", "no":
			return false, true
		}
		u.print("ответьте д или н\n")
	}
}

// input переспрашивает, пока строка не пройдёт проверку.
// Пустой ответ — отмена, тогда вернётся "".
func (u ui) input(prompt string, check func(string) error) (string, bool) {
	for {
		s, ok := u.ask(prompt)
		if !ok || s == "" {
			return "", ok
		}
		if err := check(s); err != nil {
			u.printf("%v\n", err)
			continue
		}
		return s, true
	}
}

// pickIdx спрашивает номер из списка. Пустой ответ — отмена, тогда -1.
func (u ui) pickIdx(n int) (int, bool) {
	if n == 0 {
		u.print("список пуст\n")
		return -1, u.pause()
	}
	for {
		s, ok := u.ask("номер (пусто — отмена): ")
		if !ok {
			return -1, false
		}
		if s == "" {
			return -1, true
		}
		i, err := strconv.Atoi(s)
		if err != nil || i < 1 || i > n {
			u.printf("нужен номер от 1 до %d\n", n)
			continue
		}
		return i - 1, true
	}
}

// ─── меню сервера ───────────────────────────────────────────────────────────

type serverMenu struct {
	ui
	path  string
	cfg   config.Config
	dirty bool
}

func defaultServer() config.Config {
	return config.Config{
		Listen:     map[string]string{"tcp": ":9000", "udp": ":9000"},
		StatusAddr: config.DefaultStatusAddr,
	}
}

func (m *serverMenu) run() {
	for {
		m.screen()
		s, ok := m.ask("выбор: ")
		if !ok {
			return
		}
		switch s {
		case "":
		case "1":
			ok = m.users()
		case "2":
			ok = m.listen()
		case "3":
			m.cfg.PinClientIP = !m.cfg.PinClientIP
			m.dirty = true
		case "4":
			ok = m.monitor(m.cfg.StatusAddr, config.DefaultStatusAddr)
		case "5":
			ok = m.save()
		case "0":
			if yes, cont := m.leave(m.dirty); yes || !cont {
				return
			}
		default:
			m.print("нет такого пункта\n")
			ok = m.pause()
		}
		if !ok {
			return
		}
	}
}

func (m *serverMenu) screen() {
	m.print(clearScr)
	m.printf("sunfire %s · шлюз\n", version.Number)
	m.printf("конфиг: %s%s\n\n", m.path, mark(m.dirty))
	m.printf("  адреса:       %s\n", listenLine(m.cfg.Listen))
	m.printf("  пользователи: %s\n", usersLine(m.cfg.Users))
	m.printf("  служба:       %s\n\n", unitState(unitServer))
	m.print("  1. Пользователи\n")
	m.print("  2. Адреса прослушивания\n")
	m.printf("  3. Привязка к адресу клиента: %s\n", onOff(m.cfg.PinClientIP))
	m.print("  4. Монитор\n")
	m.print("  5. Сохранить и перезапустить службу\n")
	m.print("  0. Выход\n\n")
}

func (m *serverMenu) users() bool {
	for {
		m.print(clearScr)
		m.print("Пользователи\n\n")
		if len(m.cfg.Users) == 0 {
			m.print("  пусто\n")
		}
		for i, u := range m.cfg.Users {
			m.printf("  %2d. %-20s %-14s %s\n", i+1, u.Name, mask(u.Token), onOff(!u.Disabled))
		}
		m.print("\n  действия:\n")
		m.print("    1. Добавить\n")
		m.print("    2. Включить или выключить\n")
		m.print("    3. Удалить\n")
		m.print("    4. Показать токен целиком\n")
		m.print("    0. Назад\n\n")

		s, ok := m.ask("выбор: ")
		if !ok {
			return false
		}
		switch s {
		case "":
		case "1":
			ok = m.addUser()
		case "2":
			ok = m.toggleUser()
		case "3":
			ok = m.delUser()
		case "4":
			ok = m.showToken()
		case "0":
			return true
		default:
			m.print("нет такого пункта\n")
			ok = m.pause()
		}
		if !ok {
			return false
		}
	}
}

func (m *serverMenu) addUser() bool {
	name, ok := m.input("имя (пусто — отмена): ", func(s string) error {
		for _, u := range m.cfg.Users {
			if u.Name == s {
				return errors.New("sunfire: такое имя уже есть")
			}
		}
		return nil
	})
	if !ok || name == "" {
		return ok
	}
	token, err := config.NewToken()
	if err != nil {
		return m.fail(err)
	}
	m.cfg.Users = append(m.cfg.Users, config.User{Name: name, Token: token})
	m.dirty = true
	m.printf("\nдобавлен %s\n", name)
	m.clientLine(token)
	return m.pause()
}

func (m *serverMenu) toggleUser() bool {
	i, ok := m.pickIdx(len(m.cfg.Users))
	if !ok || i < 0 {
		return ok
	}
	m.cfg.Users[i].Disabled = !m.cfg.Users[i].Disabled
	m.dirty = true
	return true
}

func (m *serverMenu) delUser() bool {
	i, ok := m.pickIdx(len(m.cfg.Users))
	if !ok || i < 0 {
		return ok
	}
	yes, ok := m.confirm("удалить " + m.cfg.Users[i].Name + "?")
	if !ok {
		return false
	}
	if yes {
		m.cfg.Users = append(m.cfg.Users[:i], m.cfg.Users[i+1:]...)
		m.dirty = true
	}
	return true
}

func (m *serverMenu) showToken() bool {
	i, ok := m.pickIdx(len(m.cfg.Users))
	if !ok || i < 0 {
		return ok
	}
	m.printf("\n%s\n", m.cfg.Users[i].Name)
	m.clientLine(m.cfg.Users[i].Token)
	return m.pause()
}

// clientLine — что вписать на стороне клиента. Свой внешний адрес шлюз не
// знает, поэтому подставляет заглушку.
func (m *serverMenu) clientLine(token string) {
	m.printf("  адрес: %s\n  токен: %s\n", m.gwHint(), token)
}

func (m *serverMenu) gwHint() string {
	addr := m.cfg.Listen["tcp"]
	if addr == "" {
		addr = m.cfg.Listen["udp"]
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "АДРЕС-СЕРВЕРА:ПОРТ"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "АДРЕС-СЕРВЕРА"
	}
	return net.JoinHostPort(host, port)
}

func (m *serverMenu) listen() bool {
	for {
		m.print(clearScr)
		m.print("Адреса прослушивания\n\n")
		m.printf("  1. tcp: %s\n", orOff(m.cfg.Listen["tcp"]))
		m.printf("  2. udp: %s\n", orOff(m.cfg.Listen["udp"]))
		m.print("\n  пустой адрес выключает транспорт\n\n  0. Назад\n\n")

		s, ok := m.ask("выбор: ")
		if !ok {
			return false
		}
		switch s {
		case "":
		case "1":
			ok = m.setListen("tcp")
		case "2":
			ok = m.setListen("udp")
		case "0":
			return true
		default:
			m.print("нет такого пункта\n")
			ok = m.pause()
		}
		if !ok {
			return false
		}
	}
}

func (m *serverMenu) setListen(kind string) bool {
	addr, ok := m.input(kind+", адрес вида :9000 (пусто — выключить): ", checkListen)
	if !ok {
		return false
	}
	if addr == "" {
		yes, ok := m.confirm("выключить " + kind + "?")
		if !ok {
			return false
		}
		if yes {
			delete(m.cfg.Listen, kind)
			m.dirty = true
		}
		return true
	}
	if m.cfg.Listen == nil {
		m.cfg.Listen = map[string]string{}
	}
	m.cfg.Listen[kind] = addr
	m.dirty = true
	return true
}

func (m *serverMenu) save() bool {
	if err := checkServer(m.cfg); err != nil {
		m.printf("%v\n", err)
		yes, ok := m.confirm("сохранить всё равно?")
		if !ok {
			return false
		}
		if !yes {
			return true
		}
	}
	if err := saveJSON(m.path, m.cfg); err != nil {
		return m.fail(err)
	}
	m.dirty = false
	m.printf("сохранено в %s\n", m.path)
	m.restart(unitServer)
	return m.pause()
}

// checkServer — то же, что проверит служба при запуске.
func checkServer(c config.Config) error {
	if len(c.Listen) == 0 {
		return errors.New("sunfire: не задан ни один адрес прослушивания")
	}
	for _, u := range c.Users {
		if !u.Disabled {
			return nil
		}
	}
	return errors.New("sunfire: нет ни одного включённого пользователя")
}

// ─── меню клиента ───────────────────────────────────────────────────────────

type clientMenu struct {
	ui
	path  string
	cfg   clientconf.Config
	dirty bool
}

func (m *clientMenu) run() {
	for {
		m.screen()
		s, ok := m.ask("выбор: ")
		if !ok {
			return
		}
		switch s {
		case "":
		case "1":
			ok = m.servers()
		case "2":
			ok = m.setSocks()
		case "3":
			ok = m.transport()
		case "4":
			ok = m.setStreams()
		case "5":
			ok = m.monitor(m.cfg.StatusAddr, clientconf.DefaultStatusAddr)
		case "6":
			ok = m.save()
		case "0":
			if yes, cont := m.leave(m.dirty); yes || !cont {
				return
			}
		default:
			m.print("нет такого пункта\n")
			ok = m.pause()
		}
		if !ok {
			return
		}
	}
}

func (m *clientMenu) screen() {
	m.print(clearScr)
	m.printf("sunfire %s · клиент\n", version.Number)
	m.printf("конфиг: %s%s\n\n", m.path, mark(m.dirty))
	m.printf("  сервер:    %s\n", activeServer(m.cfg.Servers))
	m.printf("  SOCKS:     %s\n", m.cfg.Socks)
	m.printf("  транспорт: %s\n", m.cfg.Transport)
	m.printf("  серверов:  %d\n", len(m.cfg.Servers))
	m.printf("  служба:    %s\n\n", unitState(unitClient))
	m.print("  1. Серверы\n")
	m.print("  2. Локальный SOCKS\n")
	m.print("  3. Транспорт\n")
	m.printf("  4. Соединений на сессию: %d\n", m.cfg.Streams)
	m.print("  5. Монитор\n")
	m.print("  6. Сохранить и перезапустить службу\n")
	m.print("  0. Выход\n\n")
}

func (m *clientMenu) servers() bool {
	for {
		m.print(clearScr)
		m.print("Серверы, сверху вниз — очередь перехода при отказе\n\n")
		if len(m.cfg.Servers) == 0 {
			m.print("  пусто\n")
		}
		for i, s := range m.cfg.Servers {
			m.printf("  %2d. %-16s %-24s %s\n", i+1, s.Name, s.GW, mask(s.Token))
		}
		m.print("\n  действия:\n")
		m.print("    1. Добавить\n")
		m.print("    2. Изменить\n")
		m.print("    3. Удалить\n")
		m.print("    4. Поднять\n")
		m.print("    5. Опустить\n")
		m.print("    6. Показать токен целиком\n")
		m.print("    0. Назад\n\n")

		s, ok := m.ask("выбор: ")
		if !ok {
			return false
		}
		switch s {
		case "":
		case "1":
			ok = m.addServer()
		case "2":
			ok = m.editServer()
		case "3":
			ok = m.delServer()
		case "4":
			ok = m.move(-1)
		case "5":
			ok = m.move(1)
		case "6":
			ok = m.showToken()
		case "0":
			return true
		default:
			m.print("нет такого пункта\n")
			ok = m.pause()
		}
		if !ok {
			return false
		}
	}
}

func (m *clientMenu) addServer() bool {
	name, ok := m.input("имя (пусто — отмена): ", m.freeName)
	if !ok || name == "" {
		return ok
	}
	gw, ok := m.input("адрес host:9000: ", checkGW)
	if !ok || gw == "" {
		return ok
	}
	token, ok := m.input("токен: ", checkToken)
	if !ok || token == "" {
		return ok
	}
	m.cfg.Servers = append(m.cfg.Servers, clientconf.Server{Name: name, GW: gw, Token: token})
	m.dirty = true
	return true
}

func (m *clientMenu) editServer() bool {
	i, ok := m.pickIdx(len(m.cfg.Servers))
	if !ok || i < 0 {
		return ok
	}
	s := m.cfg.Servers[i]
	m.print("\nпустой ответ оставляет прежнее\n")

	name, ok := m.input(fmt.Sprintf("имя [%s]: ", s.Name), func(v string) error {
		if v == s.Name {
			return nil
		}
		return m.freeName(v)
	})
	if !ok {
		return false
	}
	gw, ok := m.input(fmt.Sprintf("адрес [%s]: ", s.GW), checkGW)
	if !ok {
		return false
	}
	token, ok := m.input(fmt.Sprintf("токен [%s]: ", mask(s.Token)), checkToken)
	if !ok {
		return false
	}
	if name != "" {
		s.Name = name
	}
	if gw != "" {
		s.GW = gw
	}
	if token != "" {
		s.Token = token
	}
	m.cfg.Servers[i] = s
	m.dirty = true
	return true
}

func (m *clientMenu) delServer() bool {
	i, ok := m.pickIdx(len(m.cfg.Servers))
	if !ok || i < 0 {
		return ok
	}
	yes, ok := m.confirm("удалить " + m.cfg.Servers[i].Name + "?")
	if !ok {
		return false
	}
	if yes {
		m.cfg.Servers = append(m.cfg.Servers[:i], m.cfg.Servers[i+1:]...)
		m.dirty = true
	}
	return true
}

// move двигает сервер по списку: порядок и есть приоритет.
func (m *clientMenu) move(delta int) bool {
	i, ok := m.pickIdx(len(m.cfg.Servers))
	if !ok || i < 0 {
		return ok
	}
	j := i + delta
	if j < 0 || j >= len(m.cfg.Servers) {
		return true
	}
	m.cfg.Servers[i], m.cfg.Servers[j] = m.cfg.Servers[j], m.cfg.Servers[i]
	m.dirty = true
	return true
}

func (m *clientMenu) showToken() bool {
	i, ok := m.pickIdx(len(m.cfg.Servers))
	if !ok || i < 0 {
		return ok
	}
	s := m.cfg.Servers[i]
	m.printf("\n%s\n  адрес: %s\n  токен: %s\n", s.Name, s.GW, s.Token)
	return m.pause()
}

func (m *clientMenu) freeName(v string) error {
	for _, s := range m.cfg.Servers {
		if s.Name == v {
			return errors.New("sunfire: такое имя уже есть")
		}
	}
	return nil
}

func (m *clientMenu) setSocks() bool {
	addr, ok := m.input(fmt.Sprintf("адрес SOCKS [%s]: ", m.cfg.Socks), checkLoopback)
	if !ok || addr == "" {
		return ok
	}
	m.cfg.Socks = addr
	m.dirty = true
	return true
}

func (m *clientMenu) transport() bool {
	for {
		m.print(clearScr)
		m.printf("Транспорт: %s\n\n", m.cfg.Transport)
		m.print("  1. auto — замерить и взять быстрый\n")
		m.print("  2. tcp\n")
		m.print("  3. udp\n\n")
		m.print("  0. Назад\n\n")

		s, ok := m.ask("выбор: ")
		if !ok {
			return false
		}
		if t := map[string]string{"1": "auto", "2": "tcp", "3": "udp"}[s]; t != "" {
			m.cfg.Transport = t
			m.dirty = true
			return true
		}
		if s == "0" {
			return true
		}
		if s != "" {
			m.print("нет такого пункта\n")
			if !m.pause() {
				return false
			}
		}
	}
}

func (m *clientMenu) setStreams() bool {
	s, ok := m.input(fmt.Sprintf("соединений на сессию, 1 — обычный режим [%d]: ", m.cfg.Streams), checkStreams)
	if !ok || s == "" {
		return ok
	}
	m.cfg.Streams, _ = strconv.Atoi(s)
	m.dirty = true
	return true
}

func (m *clientMenu) save() bool {
	if len(m.cfg.Servers) == 0 {
		m.print("sunfire: нет ни одного сервера\n")
		yes, ok := m.confirm("сохранить всё равно?")
		if !ok {
			return false
		}
		if !yes {
			return true
		}
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return m.fail(err)
	}
	if err := m.cfg.Save(m.path); err != nil {
		return m.fail(err)
	}
	m.dirty = false
	m.printf("сохранено в %s\n", m.path)
	m.restart(unitClient)
	return m.pause()
}

func activeServer(servers []clientconf.Server) string {
	if len(servers) == 0 {
		return "не задан"
	}
	return servers[0].Name + " · " + servers[0].GW
}

// ─── общее ──────────────────────────────────────────────────────────────────

// leave — выход с главного экрана. Первое значение — уходить ли, второе —
// false на конце ввода.
func (u ui) leave(dirty bool) (bool, bool) {
	if !dirty {
		return true, true
	}
	yes, ok := u.confirm("есть несохранённые изменения, выйти?")
	return yes, ok
}

// monitor отдаёт терминал монитору и забирает обратно.
func (u ui) monitor(addr, def string) bool {
	if addr == "" {
		addr = def
	}
	// tui.Run перехватывает Ctrl+C; без сброса он и дальше не завершал бы меню
	defer signal.Reset(os.Interrupt, syscall.SIGTERM)
	if err := tui.Run(addr); err != nil {
		return u.fail(err)
	}
	return true
}

// restart перезапускает службу. Нет systemctl или отказ — сообщаем и живём дальше.
func (u ui) restart(unit string) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		u.printf("systemctl не найден, перезапустите %s сами\n", unit)
		return
	}
	out, err := exec.Command("systemctl", "restart", unit).CombinedOutput()
	if err == nil {
		u.printf("служба %s перезапущена\n", unit)
		return
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	u.printf("sunfire: не перезапустилась %s: %s\n", unit, msg)
}

// unitState — состояние службы по systemctl.
func unitState(unit string) string {
	out, err := exec.Command("systemctl", "is-active", unit).Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		if err != nil {
			return "неизвестно"
		}
		return "не установлена"
	}
	switch s {
	case "active":
		return "работает"
	case "inactive":
		return "остановлена"
	case "activating":
		return "запускается"
	case "failed":
		return "сбой"
	case "unknown":
		return "не установлена"
	}
	return s
}

// saveJSON пишет через временный файл рядом: обрыв не испортит рабочий конфиг.
// В файле лежат токены, поэтому права 0600.
func saveJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("sunfire: сборка конфига: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("sunfire: каталог %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("sunfire: временный файл рядом с %s: %w", path, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // после удачного Rename удалять уже нечего

	write := func() error {
		if err := f.Chmod(0o600); err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			return err
		}
		return f.Sync()
	}
	err = write()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("sunfire: запись %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("sunfire: замена %s: %w", path, err)
	}
	return nil
}

// hint переводит отказ по правам в понятное действие.
func hint(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w — нужен sudo", err)
	}
	return err
}

// mask — токен целиком в списке не показываем.
func mask(t string) string {
	r := []rune(t)
	if len(r) <= 12 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:6]) + "…" + string(r[len(r)-4:])
}

func mark(dirty bool) string {
	if dirty {
		return " · есть несохранённые изменения"
	}
	return ""
}

func onOff(v bool) string {
	if v {
		return "вкл"
	}
	return "выкл"
}

func orOff(addr string) string {
	if addr == "" {
		return "выключен"
	}
	return addr
}

func listenLine(listen map[string]string) string {
	var out []string
	for _, kind := range []string{"tcp", "udp"} {
		if addr := listen[kind]; addr != "" {
			out = append(out, kind+" "+addr)
		}
	}
	if len(out) == 0 {
		return "не заданы"
	}
	return strings.Join(out, " · ")
}

func usersLine(users []config.User) string {
	on := 0
	for _, u := range users {
		if !u.Disabled {
			on++
		}
	}
	return fmt.Sprintf("%d, включено %d", len(users), on)
}

func checkListen(s string) error {
	_, port, err := net.SplitHostPort(s)
	if err != nil || port == "" {
		return errors.New("sunfire: адрес вида :9000 или 1.2.3.4:9000")
	}
	return nil
}

func checkGW(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" || port == "" {
		return errors.New("sunfire: адрес вида host:9000")
	}
	return nil
}

// checkLoopback — SOCKS без авторизации, наружу его выставлять нельзя.
func checkLoopback(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil || port == "" {
		return errors.New("sunfire: адрес вида 127.0.0.1:1080")
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("sunfire: адрес обязан быть петлёй")
}

func checkToken(s string) error {
	if utf8.RuneCountInString(s) < minTokenLen {
		return fmt.Errorf("sunfire: токен короче %d символов", minTokenLen)
	}
	return nil
}

func checkStreams(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxStreams {
		return fmt.Errorf("sunfire: нужно число от 1 до %d", maxStreams)
	}
	return nil
}
