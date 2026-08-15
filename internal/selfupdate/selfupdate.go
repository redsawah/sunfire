// Package selfupdate — обновление бинаря из GitHub Releases.
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	apiURL   = "https://api.github.com/repos/redsawah/sunfire/releases/latest"
	sumsName = "SHA256SUMS"

	httpTimeout = 15 * time.Second
	maxJSON     = 1 << 20
	maxSums     = 64 << 10
	maxDeb      = 64 << 20
)

// откуда разрешено качать
var trusted = map[string]bool{
	"github.com":                    true,
	"objects.githubusercontent.com": true,
}

// Latest — что лежит в последнем релизе.
type Latest struct {
	Version string // без "v", например "0.2.0"
	Asset   string // имя .deb для этого бинаря
	URL     string // ссылка на скачивание .deb
	SHA256  string // из SHA256SUMS, пусто если не нашли
}

// Check узнаёт последнюю версию. name — "sunfire" или "sunfired".
func Check(name string) (Latest, error) {
	client := &http.Client{Timeout: httpTimeout}
	body, err := open(client, apiURL)
	if err != nil {
		return Latest{}, err
	}
	defer body.Close()

	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxJSON)).Decode(&rel); err != nil {
		return Latest{}, fmt.Errorf("sunfire: разбор ответа GitHub: %w", err)
	}

	lat := Latest{Version: strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")}
	if lat.Version == "" {
		return Latest{}, errors.New("sunfire: релиз без тега")
	}
	lat.Asset = name + "_" + lat.Version + "_amd64.deb"

	sumsURL := ""
	for _, a := range rel.Assets {
		switch a.Name {
		case lat.Asset:
			lat.URL = a.URL
		case sumsName:
			sumsURL = a.URL
		}
	}
	if lat.URL == "" {
		return Latest{}, fmt.Errorf("sunfire: в релизе нет %s", lat.Asset)
	}
	// суммы необязательны: нет — ставим без сверки
	if sumsURL != "" {
		if sums, err := fetch(client, sumsURL, maxSums); err == nil {
			lat.SHA256 = sumFor(sums, lat.Asset)
		}
	}
	return lat, nil
}

// Newer — версия a строго новее b (семвер вида X.Y.Z, нечисловое считаем равным).
func Newer(a, b string) bool {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := range 3 {
		x, okx := part(pa, i)
		y, oky := part(pb, i)
		if !okx || !oky {
			continue
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// part — i-я часть версии; отсутствующая считается нулём, нечисловая — несравнимой.
func part(p []string, i int) (int, bool) {
	if i >= len(p) {
		return 0, true
	}
	n, err := strconv.Atoi(p[i])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// Run проверяет и, если есть новее, ставит её. dry=true — только сообщить.
func Run(name, current string, dry bool, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	lat, err := Check(name)
	if err != nil {
		return err
	}
	if !Newer(lat.Version, current) {
		logf("уже последняя версия %s", current)
		return nil
	}
	if dry {
		logf("доступна %s", lat.Version)
		return nil
	}

	logf("качаю %s", lat.Asset)
	deb, err := download(lat)
	if err != nil {
		return err
	}
	defer os.Remove(deb)

	if err := install(name, deb, logf); err != nil {
		return err
	}
	logf("обновление %s поставлено, перезапустите службу: systemctl restart %s", lat.Version, name)
	return nil
}

// checkURL — качаем только по https с доменов GitHub.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("sunfire: битая ссылка")
	}
	if u.Scheme != "https" || !trusted[u.Hostname()] {
		return fmt.Errorf("sunfire: недоверенная ссылка %q", raw)
	}
	return nil
}

func open(client *http.Client, raw string) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "sunfire")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sunfire: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("sunfire: GitHub ответил %s", resp.Status)
	}
	return resp.Body, nil
}

func fetch(client *http.Client, raw string, limit int64) ([]byte, error) {
	if err := checkURL(raw); err != nil {
		return nil, err
	}
	body, err := open(client, raw)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, limit))
}

// sumFor — сумма файла из SHA256SUMS.
func sumFor(sums []byte, name string) string {
	for line := range strings.SplitSeq(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || len(f[0]) != 64 {
			continue
		}
		if strings.TrimLeft(f[1], "*./") != name {
			continue
		}
		if _, err := hex.DecodeString(f[0]); err != nil {
			continue
		}
		return strings.ToLower(f[0])
	}
	return ""
}

// download кладёт .deb во временный файл и сверяет сумму.
func download(lat Latest) (string, error) {
	if err := checkURL(lat.URL); err != nil {
		return "", err
	}
	// без общего таймаута: файл большой
	body, err := open(&http.Client{}, lat.URL)
	if err != nil {
		return "", err
	}
	defer body.Close()

	f, err := os.CreateTemp("", "sunfire-*.deb")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, maxDeb+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxDeb {
		err = errors.New("sunfire: пакет слишком велик")
	}
	if err == nil && lat.SHA256 != "" {
		if got := hex.EncodeToString(h.Sum(nil)); got != lat.SHA256 {
			err = fmt.Errorf("sunfire: sha256 не совпал: %s вместо %s", got, lat.SHA256)
		}
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func install(name, deb string, logf func(string, ...any)) error {
	dpkg, err := exec.LookPath("dpkg")
	if err != nil {
		return replaceSelf(name, deb, logf)
	}
	out, err := exec.Command(dpkg, "-i", deb).CombinedOutput()
	if s := strings.TrimSpace(string(out)); s != "" {
		logf("%s", s)
	}
	if err != nil {
		return fmt.Errorf("sunfire: dpkg: %w", err)
	}
	return nil
}

// replaceSelf заменяет только сам бинарь, взяв его из пакета.
func replaceSelf(name, deb string, logf func(string, ...any)) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return err
	}

	src, err := os.Open(deb)
	if err != nil {
		return err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(filepath.Dir(self), "."+name+"-*")
	if err != nil {
		return err
	}
	err = extractBin(src, name, tmp)
	if err == nil {
		err = tmp.Chmod(0o755)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), self)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	logf("заменён %s", self)
	return nil
}

// extractBin достаёт ./usr/bin/<name> из .deb.
func extractBin(deb io.Reader, name string, dst io.Writer) error {
	data, err := arMember(deb, "data.tar.gz")
	if err != nil {
		return err
	}
	zr, err := gzip.NewReader(data)
	if err != nil {
		return fmt.Errorf("sunfire: data.tar.gz: %w", err)
	}
	defer zr.Close()

	want := "usr/bin/" + name
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("sunfire: data.tar.gz: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if strings.TrimPrefix(strings.TrimPrefix(h.Name, "./"), "/") != want {
			continue
		}
		if h.Size > maxDeb {
			return errors.New("sunfire: бинарь слишком велик")
		}
		_, err = io.Copy(dst, tr)
		return err
	}
	return fmt.Errorf("sunfire: в пакете нет %s", want)
}

// arMember — поток данных члена ar-архива. Заголовок 60 байт, члены выровнены до чётного.
func arMember(r io.Reader, want string) (io.Reader, error) {
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, errors.New("sunfire: это не .deb")
	}
	if string(magic[:]) != "!<arch>\n" {
		return nil, errors.New("sunfire: это не .deb")
	}
	for {
		var h [60]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return nil, fmt.Errorf("sunfire: в пакете нет %s", want)
		}
		if string(h[58:60]) != "`\n" {
			return nil, errors.New("sunfire: битый ar")
		}
		size, err := strconv.ParseInt(strings.TrimSpace(string(h[48:58])), 10, 64)
		if err != nil || size < 0 {
			return nil, errors.New("sunfire: битый ar")
		}
		if strings.TrimRight(strings.TrimSpace(string(h[0:16])), "/") == want {
			return io.LimitReader(r, size), nil
		}
		if _, err := io.CopyN(io.Discard, r, size+size%2); err != nil {
			return nil, fmt.Errorf("sunfire: в пакете нет %s", want)
		}
	}
}
