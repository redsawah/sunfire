package clientconf_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sunfire/internal/clientconf"
)

const token = "0123456789abcdefghij"

func write(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.conf")
	c := clientconf.Default()
	c.Transport = "udp"
	c.Streams = 4
	c.Servers = []clientconf.Server{
		{Name: "ams", GW: "10.0.0.1:9000", Token: token},
		{Name: "fra", GW: "10.0.0.2:9000", Token: token + "x"},
	}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("права %v, ждали 0600", fi.Mode().Perm())
	}

	got, err := clientconf.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Socks != c.Socks || got.StatusAddr != c.StatusAddr ||
		got.Transport != "udp" || got.Streams != 4 || len(got.Servers) != 2 {
		t.Fatalf("после круга: %+v", got)
	}
	if got.Servers[0] != c.Servers[0] || got.Servers[1] != c.Servers[1] {
		t.Fatalf("серверы разъехались: %+v", got.Servers)
	}

	// повторное сохранение поверх существующего файла
	got.Servers = got.Servers[:1]
	if err := got.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := clientconf.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Servers) != 1 {
		t.Fatalf("серверов %d, ждали 1", len(again.Servers))
	}
	if names, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*")); len(names) != 0 {
		t.Fatalf("остались временные файлы: %v", names)
	}
}

func TestDefaults(t *testing.T) {
	path := write(t, "client.conf", `{"servers":[{"name":"ams","gw":"10.0.0.1:9000","token":"`+token+`"}]}`)
	c, err := clientconf.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Socks != clientconf.DefaultSocks {
		t.Fatalf("socks %q", c.Socks)
	}
	if c.StatusAddr != clientconf.DefaultStatusAddr {
		t.Fatalf("status_addr %q", c.StatusAddr)
	}
	if c.Transport != "auto" {
		t.Fatalf("transport %q", c.Transport)
	}
	if c.Streams != 1 {
		t.Fatalf("streams %d", c.Streams)
	}
}

func TestLoadOtkaz(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"socks наружу", `{"socks":"0.0.0.0:1080","servers":[{"name":"a","gw":"h:1","token":"` + token + `"}]}`},
		{"статус наружу", `{"status_addr":"192.168.1.5:9602","servers":[{"name":"a","gw":"h:1","token":"` + token + `"}]}`},
		{"socks без порта", `{"socks":"127.0.0.1","servers":[{"name":"a","gw":"h:1","token":"` + token + `"}]}`},
		{"короткий токен", `{"servers":[{"name":"a","gw":"h:1","token":"abc123"}]}`},
		{"короткий токен кириллицей", `{"servers":[{"name":"a","gw":"h:1","token":"короткий"}]}`},
		{"дубль имён", `{"servers":[{"name":"a","gw":"h:1","token":"` + token + `"},{"name":"a","gw":"h:2","token":"` + token + `"}]}`},
		{"пустое имя", `{"servers":[{"name":" ","gw":"h:1","token":"` + token + `"}]}`},
		{"пустой адрес", `{"servers":[{"name":"a","gw":"","token":"` + token + `"}]}`},
		{"нет серверов", `{"servers":[]}`},
		{"чужой транспорт", `{"transport":"quic","servers":[{"name":"a","gw":"h:1","token":"` + token + `"}]}`},
		{"streams меньше 1", `{"streams":-2,"servers":[{"name":"a","gw":"h:1","token":"` + token + `"}]}`},
		{"не json", `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := write(t, "client.conf", tc.body)
			c, err := clientconf.Load(path)
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
	if _, err := clientconf.Load(filepath.Join(t.TempDir(), "нет.conf")); err == nil {
		t.Fatal("ждали ошибку")
	}
}

func TestFromEnvFile(t *testing.T) {
	path := write(t, "client.env", "# старая установка\n\nGW=gw.example:9000\nexport TOKEN=\""+token+"\"\nSOCKS='127.0.0.1:1081'\nМУСОР\n")
	c, err := clientconf.FromEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Socks != "127.0.0.1:1081" || c.Transport != "auto" || c.Streams != 1 {
		t.Fatalf("конфиг %+v", c)
	}
	if len(c.Servers) != 1 {
		t.Fatalf("серверов %d, ждали 1", len(c.Servers))
	}
	s := c.Servers[0]
	if s.GW != "gw.example:9000" || s.Token != token || s.Name != "gw.example" {
		t.Fatalf("сервер %+v", s)
	}
}

func TestFromEnvFileDefaults(t *testing.T) {
	path := write(t, "client.env", "GW=10.0.0.1:9000\nTOKEN="+token+"\n")
	c, err := clientconf.FromEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Socks != clientconf.DefaultSocks || c.StatusAddr != clientconf.DefaultStatusAddr {
		t.Fatalf("умолчания не применились: %+v", c)
	}
}

func TestFromEnvFileOtkaz(t *testing.T) {
	cases := map[string]string{
		"без токена":     "GW=10.0.0.1:9000\n",
		"без адреса":     "TOKEN=" + token + "\n",
		"короткий токен": "GW=10.0.0.1:9000\nTOKEN=abc\n",
		"socks наружу":   "GW=10.0.0.1:9000\nTOKEN=" + token + "\nSOCKS=0.0.0.0:1080\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := write(t, "client.env", body)
			if _, err := clientconf.FromEnvFile(path); err == nil {
				t.Fatal("ждали ошибку")
			}
		})
	}
}

func TestDefault(t *testing.T) {
	c := clientconf.Default()
	if c.Socks != clientconf.DefaultSocks || c.StatusAddr != clientconf.DefaultStatusAddr ||
		c.Transport != "auto" || c.Streams != 1 || len(c.Servers) != 0 {
		t.Fatalf("умолчания %+v", c)
	}
}
