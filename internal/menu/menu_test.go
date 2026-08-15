package menu

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sunfire/internal/clientconf"
	"sunfire/internal/config"
)

// script — меню с заранее набранным вводом.
func script(lines ...string) (ui, *bytes.Buffer) {
	out := &bytes.Buffer{}
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	return ui{in: bufio.NewScanner(in), out: out}, out
}

func TestMask(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"короткий":             "********",
		"0123456789abcdefghij": "012345…ghij",
		"abcdefghijkl":         "************",
		"пароль-длинный-достаточно-длинный": "пароль…нный",
	}
	for in, want := range cases {
		if got := mask(in); got != want {
			t.Errorf("mask(%q) = %q, хотели %q", in, got, want)
		}
	}
}

func TestChecks(t *testing.T) {
	cases := []struct {
		name string
		fn   func(string) error
		val  string
		ok   bool
	}{
		{"listen", checkListen, ":9000", true},
		{"listen", checkListen, "1.2.3.4:9000", true},
		{"listen", checkListen, "9000", false},
		{"listen", checkListen, ":", false},
		{"gw", checkGW, "gw.example:9000", true},
		{"gw", checkGW, ":9000", false},
		{"gw", checkGW, "gw.example", false},
		{"loopback", checkLoopback, "127.0.0.1:1080", true},
		{"loopback", checkLoopback, "localhost:1080", true},
		{"loopback", checkLoopback, "0.0.0.0:1080", false},
		{"loopback", checkLoopback, "1080", false},
		{"token", checkToken, "0123456789abcdef", true},
		{"token", checkToken, "0123456789abcde", false},
		{"token", checkToken, "токен-из-шестнадц", true},
		{"token", checkToken, "токен", false},
		{"streams", checkStreams, "1", true},
		{"streams", checkStreams, "16", true},
		{"streams", checkStreams, "0", false},
		{"streams", checkStreams, "17", false},
		{"streams", checkStreams, "два", false},
	}
	for _, c := range cases {
		err := c.fn(c.val)
		if (err == nil) != c.ok {
			t.Errorf("%s(%q): err = %v, ждали ok = %v", c.name, c.val, err, c.ok)
		}
	}
}

func TestCheckServer(t *testing.T) {
	if err := checkServer(config.Config{}); err == nil {
		t.Fatal("пустой конфиг прошёл проверку")
	}
	c := defaultServer()
	if err := checkServer(c); err == nil {
		t.Fatal("конфиг без пользователей прошёл проверку")
	}
	c.Users = []config.User{{Name: "a", Token: "0123456789abcdef", Disabled: true}}
	if err := checkServer(c); err == nil {
		t.Fatal("конфиг с одними выключенными прошёл проверку")
	}
	c.Users[0].Disabled = false
	if err := checkServer(c); err != nil {
		t.Fatalf("рабочий конфиг не прошёл: %v", err)
	}
}

func TestSaveJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "main.conf")
	cfg := defaultServer()
	cfg.Users = []config.User{{Name: "node-a", Token: "0123456789abcdefgh"}}
	if err := saveJSON(path, cfg); err != nil {
		t.Fatalf("saveJSON: %v", err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got.Listen["tcp"] != cfg.Listen["tcp"] || len(got.Users) != 1 || got.Users[0].Name != "node-a" {
		t.Fatalf("прочли не то: %+v", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("права %v, хотели 0600", fi.Mode().Perm())
	}

	cfg.Users[0].Name = "node-b"
	if err := saveJSON(path, cfg); err != nil {
		t.Fatalf("перезапись: %v", err)
	}
	names, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Fatalf("рядом остался мусор: %v", names)
	}
}

func TestServerUsers(t *testing.T) {
	u, out := script(
		"9", "", // нет такого пункта
		"1", "боб", "", // добавить
		"1", "боб", "", // имя занято, отмена
		"4", "1", "", // показать токен целиком
		"2", "1", // выключить
		"3", "1", "д", // удалить
		"0",
	)
	m := &serverMenu{ui: u, cfg: defaultServer()}
	if !m.users() {
		t.Fatal("подменю оборвалось")
	}
	if len(m.cfg.Users) != 0 {
		t.Fatalf("пользователь остался: %+v", m.cfg.Users)
	}
	if !m.dirty {
		t.Fatal("изменения не отмечены")
	}
	if !strings.Contains(out.String(), "нет такого пункта") {
		t.Error("мусорный ввод прошёл молча")
	}
	if !strings.Contains(out.String(), "такое имя уже есть") {
		t.Error("двойное имя прошло молча")
	}
}

func TestServerUsersToggle(t *testing.T) {
	u, _ := script("1", "боб", "", "2", "1", "0")
	m := &serverMenu{ui: u, cfg: defaultServer()}
	m.users()
	if len(m.cfg.Users) != 1 || !m.cfg.Users[0].Disabled {
		t.Fatalf("не выключился: %+v", m.cfg.Users)
	}
	if len(m.cfg.Users[0].Token) < minTokenLen {
		t.Fatalf("короткий токен: %q", m.cfg.Users[0].Token)
	}
}

func TestServerListen(t *testing.T) {
	u, _ := script(
		"1", "9000", ":8443", // адрес без порта, потом верный
		"2", "", "д", // выключить udp
		"0",
	)
	m := &serverMenu{ui: u, cfg: defaultServer()}
	if !m.listen() {
		t.Fatal("подменю оборвалось")
	}
	if m.cfg.Listen["tcp"] != ":8443" {
		t.Fatalf("tcp = %q", m.cfg.Listen["tcp"])
	}
	if _, ok := m.cfg.Listen["udp"]; ok {
		t.Fatal("udp не выключился")
	}
}

func TestClientServers(t *testing.T) {
	u, _ := script(
		"1", "дом", "гв", "1.2.3.4:9000", "коротко", "0123456789abcdef", // адрес и токен с ошибкой
		"1", "запас", "5.6.7.8:9000", "fedcba9876543210",
		"4", "2", // поднять запасной
		"6", "1", "", // показать токен целиком
		"0",
	)
	m := &clientMenu{ui: u, cfg: clientconf.Default()}
	if !m.servers() {
		t.Fatal("подменю оборвалось")
	}
	if len(m.cfg.Servers) != 2 {
		t.Fatalf("серверов %d: %+v", len(m.cfg.Servers), m.cfg.Servers)
	}
	if m.cfg.Servers[0].Name != "запас" || m.cfg.Servers[1].Name != "дом" {
		t.Fatalf("порядок не тот: %+v", m.cfg.Servers)
	}
	if m.cfg.Servers[1].GW != "1.2.3.4:9000" {
		t.Fatalf("адрес не тот: %+v", m.cfg.Servers[1])
	}
}

func TestClientServerEdit(t *testing.T) {
	u, _ := script(
		"2", "1", "", "9.9.9.9:9000", "", // меняем только адрес
		"3", "1", "д", // удалить
		"3", "1", // список пуст
		"", "0",
	)
	m := &clientMenu{ui: u, cfg: clientconf.Config{
		Servers: []clientconf.Server{{Name: "дом", GW: "1.2.3.4:9000", Token: "0123456789abcdef"}},
	}}
	if !m.servers() {
		t.Fatal("подменю оборвалось")
	}
	if len(m.cfg.Servers) != 0 {
		t.Fatalf("сервер остался: %+v", m.cfg.Servers)
	}
}

func TestClientMisc(t *testing.T) {
	u, _ := script(
		"8.8.8.8:1080", "127.0.0.1:1081", // не петля, потом петля
		"2",       // транспорт tcp
		"99", "4", // соединений: мимо, потом верно
	)
	m := &clientMenu{ui: u, cfg: clientconf.Default()}
	if !m.setSocks() || m.cfg.Socks != "127.0.0.1:1081" {
		t.Fatalf("socks = %q", m.cfg.Socks)
	}
	if !m.transport() || m.cfg.Transport != "tcp" {
		t.Fatalf("транспорт = %q", m.cfg.Transport)
	}
	if !m.setStreams() || m.cfg.Streams != 4 {
		t.Fatalf("streams = %d", m.cfg.Streams)
	}
}

// Конец ввода — тихий выход, а не паника.
func TestEOF(t *testing.T) {
	u, _ := script("1", "дом")
	m := &clientMenu{ui: u, cfg: clientconf.Default()}
	if m.servers() {
		t.Fatal("конец ввода не заметили")
	}

	u2, _ := script("1")
	s := &serverMenu{ui: u2, cfg: defaultServer()}
	if s.users() {
		t.Fatal("конец ввода не заметили")
	}
	if yes, ok := s.leave(true); ok || yes {
		t.Fatal("на конце ввода подтверждение не спрашивают")
	}
}

func TestLeave(t *testing.T) {
	u, _ := script("н", "д")
	if yes, ok := u.leave(false); !yes || !ok {
		t.Fatal("без изменений выход не должен спрашивать")
	}
	if yes, ok := u.leave(true); yes || !ok {
		t.Fatal("ответ «нет» должен оставлять в меню")
	}
	if yes, ok := u.leave(true); !yes || !ok {
		t.Fatal("ответ «да» должен выпускать")
	}
}

func TestCreateClient(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, "client.env")
	old := "GW=gw.example:9000\nTOKEN=\"0123456789abcdef\"\nSOCKS=127.0.0.1:1081\n"
	if err := os.WriteFile(env, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	u, _ := script("д")
	m := &clientMenu{ui: u, path: filepath.Join(dir, "client.conf")}
	if !m.create(env) {
		t.Fatal("перенос не состоялся")
	}
	if len(m.cfg.Servers) != 1 || m.cfg.Servers[0].GW != "gw.example:9000" || m.cfg.Socks != "127.0.0.1:1081" {
		t.Fatalf("перенесли не то: %+v", m.cfg)
	}
	if !m.dirty {
		t.Error("перенос не отмечен как несохранённый")
	}

	// без старого файла остаются умолчания
	u2, _ := script("д")
	m2 := &clientMenu{ui: u2, path: filepath.Join(dir, "client.conf")}
	if !m2.create(filepath.Join(dir, "нет.env")) {
		t.Fatal("умолчания не завелись")
	}
	if m2.cfg.Socks != clientconf.DefaultSocks || m2.cfg.Transport != "auto" {
		t.Fatalf("не умолчания: %+v", m2.cfg)
	}

	u3, _ := script("н", "н")
	m3 := &clientMenu{ui: u3, path: filepath.Join(dir, "client.conf")}
	if m3.create(env) {
		t.Fatal("отказ должен выпускать из меню")
	}
}

func TestGWHint(t *testing.T) {
	m := &serverMenu{cfg: config.Config{Listen: map[string]string{"tcp": ":9000"}}}
	if got := m.gwHint(); got != "АДРЕС-СЕРВЕРА:9000" {
		t.Errorf("gwHint = %q", got)
	}
	m.cfg.Listen = map[string]string{"udp": "1.2.3.4:9001"}
	if got := m.gwHint(); got != "1.2.3.4:9001" {
		t.Errorf("gwHint = %q", got)
	}
	m.cfg.Listen = nil
	if got := m.gwHint(); got != "АДРЕС-СЕРВЕРА:ПОРТ" {
		t.Errorf("gwHint = %q", got)
	}
}
