package proto

import (
	"io"
	"testing"
)

// countWriter считает вызовы Write и записанные байты.
type countWriter struct {
	writes int
	bytes  int
}

func (w *countWriter) Write(p []byte) (int, error) {
	w.writes++
	w.bytes += len(p)
	return len(p), nil
}

func (w *countWriter) Read([]byte) (int, error) { return 0, io.EOF }
func (w *countWriter) Close() error             { return nil }

// Кадр обязан уходить одним Write: раздельная запись заголовка гнала бы на
// провод отдельный крошечный сегмент перед каждыми 16 КиБ данных.
func TestFrameSingleWrite(t *testing.T) {
	w := &countWriter{}
	c, err := Dial(w, "s3cr3t-token-длинный", "example.com:443")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Dial: соль одним Write плюс первый кадр одним Write
	if w.writes != 2 {
		t.Fatalf("рукопожатие: Write вызван %d раз, ожидалось 2", w.writes)
	}

	before := w.writes
	payload := make([]byte, 3*MaxPayload) // ровно три кадра
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := w.writes - before; got != 3 {
		t.Fatalf("три кадра ушли за %d вызовов Write, ожидалось 3", got)
	}
}

// Установившийся поток не должен аллоцировать на каждый кадр: буферы
// переиспользуются, иначе на гигабитах вся работа уходит в сборщик мусора.
func TestFrameWriteAllocs(t *testing.T) {
	w := &countWriter{}
	c, err := Dial(w, "s3cr3t-token-длинный", "example.com:443")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	buf := make([]byte, MaxPayload)
	// прогреваем: первый кадр набивается сильно и растит буферы
	for i := 0; i < 20; i++ {
		if _, err := c.Write(buf); err != nil {
			t.Fatalf("прогрев: %v", err)
		}
	}
	got := testing.AllocsPerRun(200, func() {
		_, _ = c.Write(buf)
	})
	if got > 1 {
		t.Fatalf("аллокаций на кадр: %.1f, ожидалось не больше 1", got)
	}
}
