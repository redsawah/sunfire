// Package clientconf — конфигурация клиента: локальные адреса и список серверов.
package clientconf

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const DefaultPath = "/etc/sunfire/client.conf"

const (
	DefaultSocks      = "127.0.0.1:1080"
	DefaultStatusAddr = "127.0.0.1:9602"

	minTokenLen = 16
)

var transports = map[string]bool{"auto": true, "tcp": true, "udp": true}

type Server struct {
	Name  string `json:"name"` // короткое имя для меню
	GW    string `json:"gw"`   // host:port
	Token string `json:"token"`
}

type Config struct {
	Socks      string   `json:"socks"`       // только петля
	StatusAddr string   `json:"status_addr"` // только петля
	Transport  string   `json:"transport"`   // auto|tcp|udp
	Streams    int      `json:"streams"`
	Servers    []Server `json:"servers"` // порядок = приоритет фолбэка
}

func Default() Config {
	return Config{
		Socks:      DefaultSocks,
		StatusAddr: DefaultStatusAddr,
		Transport:  "auto",
		Streams:    1,
	}
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("sunfire: не читается %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("sunfire: разбор %s: %w", path, err)
	}
	if err := c.normalize(); err != nil {
		return Config{}, fmt.Errorf("sunfire: %s: %w", path, err)
	}
	return c, nil
}

// Save пишет соседний временный файл и переименовывает: оборванная запись
// не испортит рабочий конфиг. В файле лежит токен, поэтому права 0600.
func (c Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("sunfire: сборка конфига: %w", err)
	}
	data = append(data, '\n')

	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
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

// FromEnvFile читает старый /etc/sunfire/client.env (GW=, TOKEN=, SOCKS=) и
// возвращает конфиг с одним сервером. Нужно для перехода со старой установки.
func FromEnvFile(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("sunfire: не читается %s: %w", path, err)
	}
	defer f.Close()

	env := make(map[string]string, 3)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		env[strings.TrimSpace(key)] = unquote(strings.TrimSpace(val))
	}
	if err := sc.Err(); err != nil {
		return Config{}, fmt.Errorf("sunfire: чтение %s: %w", path, err)
	}

	gw, token := env["GW"], env["TOKEN"]
	if gw == "" || token == "" {
		return Config{}, fmt.Errorf("sunfire: в %s нет GW или TOKEN", path)
	}
	c := Default()
	c.Socks = env["SOCKS"]
	c.Servers = []Server{{Name: serverName(gw), GW: gw, Token: token}}
	if err := c.normalize(); err != nil {
		return Config{}, fmt.Errorf("sunfire: %s: %w", path, err)
	}
	return c, nil
}

func (c *Config) normalize() error {
	c.Socks = strings.TrimSpace(c.Socks)
	if c.Socks == "" {
		c.Socks = DefaultSocks
	}
	if err := loopbackOnly(c.Socks); err != nil {
		return fmt.Errorf("socks: %w", err)
	}

	c.StatusAddr = strings.TrimSpace(c.StatusAddr)
	if c.StatusAddr == "" {
		c.StatusAddr = DefaultStatusAddr
	}
	if err := loopbackOnly(c.StatusAddr); err != nil {
		return fmt.Errorf("status_addr: %w", err)
	}

	c.Transport = strings.TrimSpace(c.Transport)
	if c.Transport == "" {
		c.Transport = "auto"
	}
	if !transports[c.Transport] {
		return fmt.Errorf("неизвестный транспорт %q", c.Transport)
	}

	if c.Streams == 0 {
		c.Streams = 1
	}
	if c.Streams < 1 {
		return fmt.Errorf("streams: %d, нужно не меньше 1", c.Streams)
	}

	if len(c.Servers) == 0 {
		return errors.New("нет ни одного сервера")
	}
	seen := make(map[string]bool, len(c.Servers))
	for i := range c.Servers {
		s := &c.Servers[i]
		s.Name = strings.TrimSpace(s.Name)
		s.GW = strings.TrimSpace(s.GW)
		s.Token = strings.TrimSpace(s.Token)
		if s.Name == "" {
			return fmt.Errorf("у сервера №%d пустое имя", i+1)
		}
		if s.GW == "" {
			return fmt.Errorf("у сервера %q пустой адрес", s.Name)
		}
		if s.Token == "" {
			return fmt.Errorf("у сервера %q пустой токен", s.Name)
		}
		if utf8.RuneCountInString(s.Token) < minTokenLen {
			return fmt.Errorf("токен сервера %q короче %d символов", s.Name, minTokenLen)
		}
		if seen[s.Name] {
			return fmt.Errorf("имя сервера %q повторяется", s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}

// loopbackOnly — SOCKS и статус без авторизации, наружу их выставлять нельзя.
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("адрес вида 127.0.0.1:1080: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("адрес обязан быть петлёй, а не %q", host)
}

// serverName — имя для меню из адреса шлюза.
func serverName(gw string) string {
	if host, _, err := net.SplitHostPort(gw); err == nil && host != "" {
		return host
	}
	return gw
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
