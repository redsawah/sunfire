package config_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sunfire/internal/config"
)

const (
	token = "0123456789abcdefghij"
	key   = "0123456789abcdefghijklmn"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "main.conf")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	path := write(t, `{
		"listen": {"tcp": ":9000", "udp": " :9001 "},
		"transport_key": " `+key+` ",
		"users": [
			{"name": "a", "token": "`+token+`"},
			{"name": "b", "token": "`+token+`x", "disabled": true}
		]
	}`)
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen["udp"] != ":9001" {
		t.Fatalf("udp %q", c.Listen["udp"])
	}
	if c.TransportKey != key {
		t.Fatalf("transport_key %q", c.TransportKey)
	}
	if c.StatusAddr != config.DefaultStatusAddr {
		t.Fatalf("status_addr %q", c.StatusAddr)
	}
	if got := c.Tokens(); len(got) != 1 || got[token] != "a" {
		t.Fatalf("токены %+v", got)
	}
}

// Транспортный ключ нужен только для udp.
func TestTransportKey(t *testing.T) {
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"tcp без ключа", `{"listen":{"tcp":":9000"},"users":[{"name":"a","token":"` + token + `"}]}`, true},
		{"udp с ключом", `{"listen":{"udp":":9000"},"transport_key":"` + key + `","users":[{"name":"a","token":"` + token + `"}]}`, true},
		{"udp без ключа", `{"listen":{"udp":":9000"},"users":[{"name":"a","token":"` + token + `"}]}`, false},
		{"udp с пробелами вместо ключа", `{"listen":{"udp":":9000"},"transport_key":"   ","users":[{"name":"a","token":"` + token + `"}]}`, false},
		{"udp с коротким ключом", `{"listen":{"tcp":":9000","udp":":9000"},"transport_key":"коротко","users":[{"name":"a","token":"` + token + `"}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(write(t, tc.body))
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, ждали ok = %v", err, tc.ok)
			}
			if err == nil {
				return
			}
			if !strings.HasPrefix(err.Error(), "sunfire: ") {
				t.Fatalf("ошибка без префикса: %v", err)
			}
			if !strings.Contains(err.Error(), "transport_key") {
				t.Fatalf("ошибка не про ключ: %v", err)
			}
		})
	}
}

func TestLoadOtkaz(t *testing.T) {
	cases := map[string]string{
		"нет адресов":       `{"users":[{"name":"a","token":"` + token + `"}]}`,
		"чужой транспорт":   `{"listen":{"quic":":9000"},"users":[{"name":"a","token":"` + token + `"}]}`,
		"пустой адрес":      `{"listen":{"tcp":" "},"users":[{"name":"a","token":"` + token + `"}]}`,
		"статус наружу":     `{"listen":{"tcp":":9000"},"status_addr":"0.0.0.0:9601","users":[{"name":"a","token":"` + token + `"}]}`,
		"пустое имя":        `{"listen":{"tcp":":9000"},"users":[{"name":" ","token":"` + token + `"}]}`,
		"пустой токен":      `{"listen":{"tcp":":9000"},"users":[{"name":"a","token":""}]}`,
		"короткий токен":    `{"listen":{"tcp":":9000"},"users":[{"name":"a","token":"abc"}]}`,
		"дубль токенов":     `{"listen":{"tcp":":9000"},"users":[{"name":"a","token":"` + token + `"},{"name":"b","token":"` + token + `"}]}`,
		"все выключены":     `{"listen":{"tcp":":9000"},"users":[{"name":"a","token":"` + token + `","disabled":true}]}`,
		"нет пользователей": `{"listen":{"tcp":":9000"},"users":[]}`,
		"не json":           `{`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := config.Load(write(t, body))
			if err == nil {
				t.Fatalf("ждали ошибку, получили %+v", c)
			}
			if !strings.HasPrefix(err.Error(), "sunfire: ") {
				t.Fatalf("ошибка без префикса: %v", err)
			}
		})
	}
}

func TestLoadNetFayla(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "нет.conf")); err == nil {
		t.Fatal("ждали ошибку")
	}
}

func TestAllowed(t *testing.T) {
	c := config.Config{Users: []config.User{
		{Name: "везде", Token: token},
		{Name: "свой", Token: token + "x", ClientIPs: []string{"10.0.0.1", "192.168.0.0/24"}},
	}}
	cases := []struct {
		user string
		ip   string
		ok   bool
	}{
		{"везде", "8.8.8.8", true},
		{"свой", "10.0.0.1", true},
		{"свой", "192.168.0.7", true},
		{"свой", "10.0.0.2", false},
		{"чужой", "10.0.0.1", false},
	}
	for _, tc := range cases {
		if got := c.Allowed(tc.user, net.ParseIP(tc.ip)); got != tc.ok {
			t.Errorf("Allowed(%q, %s) = %v", tc.user, tc.ip, got)
		}
	}
}

func TestNewSecrets(t *testing.T) {
	tok, err := config.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	k1, err := config.NewTransportKey()
	if err != nil {
		t.Fatal(err)
	}
	k2, err := config.NewTransportKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(k1) != 32 || len(tok) != 32 {
		t.Fatalf("длины: токен %d, ключ %d", len(tok), len(k1))
	}
	if k1 == k2 {
		t.Fatal("ключи повторяются")
	}
}
