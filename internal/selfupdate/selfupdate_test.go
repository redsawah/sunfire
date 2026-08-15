package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.0", true},
		{"0.10.0", "0.9.0", true},
		{"0.9.0", "0.10.0", false},
		{"1.0.0", "0.9.9", true},
		{"0.2.1", "0.2.0", true},
		{"0.2.0", "0.2.1", false},
		{"0.2.0", "0.2.0", false},
		{"0.2", "0.2.0", false},
		{"0.2.0", "0.2", false},
		{"v0.3.0", "0.2.9", true},
		{"0.2.0", "dev", true},
		{"dev", "0.2.0", false},
		{"dev", "dev", false},
		{"0.2.0", "0.2.0-rc1", false},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestSumFor(t *testing.T) {
	const sum = "3b1f2c4d5e6a7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e"
	sums := strings.Join([]string{
		"# комментарий",
		sum + "  sunfired_0.2.0_amd64.deb",
		strings.ToUpper(sum) + " *sunfire_0.2.0_amd64.deb",
		"короткая  sunfire_0.3.0_amd64.deb",
		"zzzz2c4d5e6a7b8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e  bad.deb",
		"",
	}, "\n")

	if got := sumFor([]byte(sums), "sunfired_0.2.0_amd64.deb"); got != sum {
		t.Errorf("шлюз: %q", got)
	}
	if got := sumFor([]byte(sums), "sunfire_0.2.0_amd64.deb"); got != sum {
		t.Errorf("клиент: %q", got)
	}
	for _, name := range []string{"sunfire_0.3.0_amd64.deb", "bad.deb", "нет такого"} {
		if got := sumFor([]byte(sums), name); got != "" {
			t.Errorf("%s: %q", name, got)
		}
	}
}

func TestCheckURL(t *testing.T) {
	ok := []string{
		"https://github.com/redsawah/sunfire/releases/download/v0.2.0/sunfire_0.2.0_amd64.deb",
		"https://objects.githubusercontent.com/x/y",
	}
	for _, u := range ok {
		if err := checkURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []string{
		"http://github.com/a",
		"https://github.com.evil.tld/a",
		"https://evil.tld/github.com",
		"ftp://github.com/a",
		"",
	}
	for _, u := range bad {
		if err := checkURL(u); err == nil {
			t.Errorf("принята ссылка %q", u)
		}
	}
}

// arPut пишет член ar-архива с выравниванием до чётного байта.
func arPut(b *bytes.Buffer, name string, data []byte) {
	fmt.Fprintf(b, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(data))
	b.Write(data)
	if len(data)%2 == 1 {
		b.WriteByte('\n')
	}
}

func tgz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "./usr/bin/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		h := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func makeDeb(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("!<arch>\n")
	arPut(&b, "debian-binary", []byte("2.0\n"))
	arPut(&b, "control.tar.gz", tgz(t, map[string][]byte{"./control": []byte("Package: sunfired\n")}))
	arPut(&b, "odd", []byte("abc")) // нечётный член — проверка выравнивания
	arPut(&b, "data.tar.gz", tgz(t, files))
	return b.Bytes()
}

func TestExtractBin(t *testing.T) {
	want := bytes.Repeat([]byte("ELF"), 1000)
	deb := makeDeb(t, map[string][]byte{
		"./usr/bin/sunfired":                  want,
		"./usr/bin/sunfire":                   []byte("другой"),
		"./lib/systemd/system/sunfired.servi": []byte("[Unit]\n"),
	})

	var out bytes.Buffer
	if err := extractBin(bytes.NewReader(deb), "sunfired", &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("получено %d байт из %d", out.Len(), len(want))
	}

	if err := extractBin(bytes.NewReader(deb), "нетакого", &bytes.Buffer{}); err == nil {
		t.Fatal("нашёлся отсутствующий бинарь")
	}
}

func TestArMemberBad(t *testing.T) {
	deb := makeDeb(t, map[string][]byte{"./usr/bin/sunfire": []byte("x")})

	if _, err := arMember(bytes.NewReader([]byte("не архив")), "data.tar.gz"); err == nil {
		t.Fatal("принят чужой формат")
	}
	if _, err := arMember(bytes.NewReader(deb[:40]), "data.tar.gz"); err == nil {
		t.Fatal("принят обрезанный архив")
	}
	if _, err := arMember(bytes.NewReader(deb), "нет.tar.gz"); err == nil {
		t.Fatal("нашёлся отсутствующий член")
	}

	broken := bytes.Clone(deb)
	broken[8+58] = 'x' // портим магию заголовка
	if _, err := arMember(bytes.NewReader(broken), "data.tar.gz"); err == nil {
		t.Fatal("принят битый заголовок")
	}
}
