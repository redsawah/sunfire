// Package config — конфигурация шлюза.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	minTokenLen = 16
	minKeyLen   = 16
	tokenLen    = 32
)

var listenKinds = map[string]bool{"tcp": true, "udp": true}

type User struct {
	Name  string `json:"name"`
	Token string `json:"token"`
	// ClientIPs — с каких адресов принимать этот токен. Пусто = откуда угодно.
	// Ограничения на то, куда клиент ходит, нет и не предполагается.
	ClientIPs []string `json:"client_ips,omitempty"`
	// Clients — сколько РАЗНЫХ машин может работать под этим токеном
	// одновременно. 0 — без ограничения. Машина с десятком соединений
	// занимает одно место, а не десять.
	Clients  int  `json:"clients,omitempty"`
	Disabled bool `json:"disabled,omitempty"`
}

// DefaultStatusAddr — адрес статуса для монитора, по умолчанию.
const DefaultStatusAddr = "127.0.0.1:9601"

type Config struct {
	Listen map[string]string `json:"listen"` // транспорт -> адрес, напр. {"tcp":":9000","udp":":9001"}
	// TransportKey — общий транспортный ключ шлюза, нужен для udp: датаграмму
	// приходится расшифровать раньше, чем станет известно, чей это пользователь.
	// Пользователей различает токен внутри потока.
	TransportKey string `json:"transport_key,omitempty"`
	// StatusAddr — статус для монитора (sunfired -top). Только петля.
	StatusAddr string `json:"status_addr,omitempty"`
	// PinClientIP — привязать токен к первому адресу, с которого им воспользовались.
	// Удобно, когда адрес клиента заранее неизвестен: шлюз запомнит его сам.
	PinClientIP bool   `json:"pin_client_ip,omitempty"`
	Users       []User `json:"users"`
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

func (c *Config) normalize() error {
	if len(c.Listen) == 0 {
		return errors.New("не задан ни один адрес прослушивания")
	}
	listen := make(map[string]string, len(c.Listen))
	for kind, addr := range c.Listen {
		kind = strings.TrimSpace(kind)
		if !listenKinds[kind] {
			return fmt.Errorf("неизвестный транспорт %q", kind)
		}
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return fmt.Errorf("пустой адрес для транспорта %q", kind)
		}
		listen[kind] = addr
	}
	c.Listen = listen

	c.TransportKey = strings.TrimSpace(c.TransportKey)
	if _, udp := c.Listen["udp"]; udp {
		if c.TransportKey == "" {
			return errors.New("для udp нужен transport_key (сгенерировать: sunfired -menu)")
		}
		if utf8.RuneCountInString(c.TransportKey) < minKeyLen {
			return fmt.Errorf("transport_key короче %d символов", minKeyLen)
		}
	}

	c.StatusAddr = strings.TrimSpace(c.StatusAddr)
	if c.StatusAddr == "" {
		c.StatusAddr = DefaultStatusAddr
	}
	if err := loopbackOnly(c.StatusAddr); err != nil {
		return fmt.Errorf("status_addr: %w", err)
	}

	seen := make(map[string]bool, len(c.Users))
	enabled := 0
	for i := range c.Users {
		u := &c.Users[i]
		u.Name = strings.TrimSpace(u.Name)
		u.Token = strings.TrimSpace(u.Token)
		if u.Name == "" {
			return fmt.Errorf("у пользователя №%d пустое имя", i+1)
		}
		if u.Token == "" {
			return fmt.Errorf("пустой токен у пользователя %q", u.Name)
		}
		if len(u.Token) < minTokenLen {
			return fmt.Errorf("токен пользователя %q короче %d символов", u.Name, minTokenLen)
		}
		if seen[u.Token] {
			return fmt.Errorf("токен пользователя %q повторяется", u.Name)
		}
		seen[u.Token] = true
		if u.Clients < 0 {
			return fmt.Errorf("clients у пользователя %q отрицателен", u.Name)
		}
		for j, rule := range u.ClientIPs {
			u.ClientIPs[j] = strings.TrimSpace(rule)
		}
		if !u.Disabled {
			enabled++
		}
	}
	if enabled == 0 {
		return errors.New("нет ни одного включённого пользователя")
	}
	return nil
}

// Tokens возвращает token -> имя пользователя (только включённые).
func (c Config) Tokens() map[string]string {
	m := make(map[string]string, len(c.Users))
	for _, u := range c.Users {
		if u.Disabled {
			continue
		}
		m[u.Token] = u.Name
	}
	return m
}

// Allowed проверяет, разрешён ли адрес для пользователя (пустой список = отовсюду).
func (c Config) Allowed(user string, ip net.IP) bool {
	for _, u := range c.Users {
		if u.Name != user {
			continue
		}
		if len(u.ClientIPs) == 0 {
			return true
		}
		for _, rule := range u.ClientIPs {
			if matchIP(rule, ip) {
				return true
			}
		}
		return false
	}
	return false
}

// Limit — сколько разных машин пускать под токеном пользователя (0 — без ограничения).
func (c Config) Limit(user string) int {
	for _, u := range c.Users {
		if u.Name == user {
			return u.Clients
		}
	}
	return 0
}

// loopbackOnly — статус не выставляем наружу.
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("адрес вида 127.0.0.1:9601: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("адрес обязан быть петлёй, а не %q", host)
}

func matchIP(rule string, ip net.IP) bool {
	if rule == "" || ip == nil {
		return false
	}
	if strings.Contains(rule, "/") {
		_, netw, err := net.ParseCIDR(rule)
		return err == nil && netw.Contains(ip)
	}
	return net.ParseIP(rule).Equal(ip)
}

// NewToken — токен пользователя.
func NewToken() (string, error) { return newSecret("токена") }

// NewTransportKey — общий транспортный ключ шлюза.
func NewTransportKey() (string, error) { return newSecret("транспортного ключа") }

func newSecret(what string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("sunfire: генерация %s: %w", what, err)
	}
	return base64.RawURLEncoding.EncodeToString(b)[:tokenLen], nil
}
