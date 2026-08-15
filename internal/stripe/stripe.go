// Package stripe — раскладка одного потока по нескольким соединениям.
package stripe

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ChunkSize = 64 * 1024

	hdrLen   = 4
	maxChunk = 8 << 20
	sidLen   = 16
	queueLen = 4
)

// вид первого кадра
const (
	KindPlain = iota
	KindStart
	KindJoin
)

var (
	errClosed = errors.New("sunfire: раскладка закрыта")
	errHead   = errors.New("sunfire: испорченный первый кадр")
	errChunk  = errors.New("sunfire: недопустимая длина куска")
)

// NewSessionID — 16 случайных байт в hex.
func NewSessionID() (string, error) {
	b := make([]byte, sidLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Head — разобранный первый кадр.
type Head struct {
	Kind   int
	SID    string
	Index  int
	Count  int
	Target string
}

func FrameStart(sid string, n int, target string) string {
	return "@s " + sid + " " + strconv.Itoa(n) + " " + target
}

func FrameJoin(sid string, idx int) string {
	return "@j " + sid + " " + strconv.Itoa(idx)
}

func validSID(s string) bool {
	if len(s) != 2*sidLen {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ParseHead разбирает первый кадр. Неизвестное — KindPlain с сырой строкой.
func ParseHead(s string) (Head, error) {
	switch {
	case strings.HasPrefix(s, "@s "):
		f := strings.SplitN(s, " ", 4)
		if len(f) != 4 || !validSID(f[1]) || f[3] == "" {
			return Head{}, errHead
		}
		n, err := strconv.Atoi(f[2])
		if err != nil || n < 1 {
			return Head{}, errHead
		}
		return Head{Kind: KindStart, SID: f[1], Count: n, Target: f[3]}, nil

	case strings.HasPrefix(s, "@j "):
		f := strings.Split(s, " ")
		if len(f) != 3 || !validSID(f[1]) {
			return Head{}, errHead
		}
		idx, err := strconv.Atoi(f[2])
		if err != nil || idx < 1 {
			return Head{}, errHead
		}
		return Head{Kind: KindJoin, SID: f[1], Index: idx}, nil
	}
	return Head{Kind: KindPlain, Target: s}, nil
}

type stripe struct {
	conns []io.ReadWriteCloser
	chans []chan []byte
	quit  chan struct{}
	wg    sync.WaitGroup
	wonce sync.Once
	once  sync.Once

	rmu  sync.Mutex
	rpos int
	rbuf []byte
	rerr error

	wmu    sync.Mutex
	wpos   int
	closed bool

	emu sync.Mutex
	err error
}

// Join склеивает соединения в один поток: кусок k едет по conns[k mod n].
func Join(conns []io.ReadWriteCloser) io.ReadWriteCloser {
	s := &stripe{
		conns: conns,
		chans: make([]chan []byte, len(conns)),
		quit:  make(chan struct{}),
	}
	for i, c := range conns {
		ch := make(chan []byte, queueLen)
		s.chans[i] = ch
		s.wg.Add(1)
		go s.writer(c, ch)
	}
	return s
}

func (s *stripe) fail(err error) {
	if err == nil {
		return
	}
	s.emu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.emu.Unlock()
}

func (s *stripe) failure() error {
	s.emu.Lock()
	defer s.emu.Unlock()
	return s.err
}

// writer пишет свою долю кусков; после закрытия канала — маркер конца.
// Сломавшись, продолжает вычитывать канал, чтобы не запереть диспетчера.
func (s *stripe) writer(c io.ReadWriteCloser, ch chan []byte) {
	defer s.wg.Done()
	broken := false
	for b := range ch {
		if broken {
			continue
		}
		if _, err := c.Write(b); err != nil {
			s.fail(err)
			broken = true
		}
	}
	if broken {
		return
	}
	if _, err := c.Write(make([]byte, hdrLen)); err != nil {
		s.fail(err)
	}
}

func (s *stripe) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed {
		return 0, errClosed
	}
	if err := s.failure(); err != nil {
		return 0, err
	}
	n := 0
	for len(p) > 0 {
		k := p
		if len(k) > ChunkSize {
			k = k[:ChunkSize]
		}
		p = p[len(k):]

		buf := make([]byte, hdrLen+len(k))
		binary.BigEndian.PutUint32(buf[:hdrLen], uint32(len(k)))
		copy(buf[hdrLen:], k)

		select {
		case s.chans[s.wpos] <- buf:
		case <-s.quit:
			return n, errClosed
		}
		s.wpos = (s.wpos + 1) % len(s.chans)
		n += len(k)
	}
	return n, s.failure()
}

func readChunk(c io.Reader) ([]byte, error) {
	var hdr [hdrLen]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, nil
	}
	if n > maxChunk {
		return nil, errChunk
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// drain дочитывает маркеры конца с остальных соединений: их пишут все писатели
// той стороны, и без вычитывания они останутся заблокированными.
func (s *stripe) drain(from int) {
	var hdr [hdrLen]byte
	for i := 1; i < len(s.conns); i++ {
		_, _ = io.ReadFull(s.conns[(from+i)%len(s.conns)], hdr[:])
	}
}

func (s *stripe) Read(p []byte) (int, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	for len(s.rbuf) == 0 {
		if s.rerr != nil {
			return 0, s.rerr
		}
		buf, err := readChunk(s.conns[s.rpos])
		if err != nil {
			s.rerr = err
			return 0, err
		}
		pos := s.rpos
		s.rpos = (s.rpos + 1) % len(s.conns)
		if len(buf) == 0 {
			s.drain(pos)
			s.rerr = io.EOF
			return 0, io.EOF
		}
		s.rbuf = buf
	}
	n := copy(p, s.rbuf)
	s.rbuf = s.rbuf[n:]
	return n, nil
}

// CloseWrite дописывает маркеры конца и ждёт, пока они уйдут. Чтение
// продолжает работать: другая сторона получает io.EOF и дописывает свою половину.
func (s *stripe) CloseWrite() error {
	s.closeWrite()
	return s.failure()
}

func (s *stripe) closeWrite() {
	s.wonce.Do(func() {
		close(s.quit)
		s.wmu.Lock()
		s.closed = true
		for _, ch := range s.chans {
			close(ch)
		}
		s.wmu.Unlock()
		s.wg.Wait()
	})
}

// closeGrace — сколько ждём, пока писатели допишут маркеры конца. Если вторая
// сторона перестала читать, запись висит вечно, поэтому по истечении срока рвём
// соединения: тогда writе вернёт ошибку и goroutine выйдут.
const closeGrace = 5 * time.Second

func (s *stripe) Close() error {
	s.once.Do(func() {
		done := make(chan struct{})
		go func() { s.closeWrite(); close(done) }()
		select {
		case <-done:
		case <-time.After(closeGrace):
		}
		for _, c := range s.conns {
			s.fail(c.Close())
		}
		<-done
	})
	return s.failure()
}
