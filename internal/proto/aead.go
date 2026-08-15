// Package proto — шифрованный потоковый канал поверх TCP.
package proto

import (
	"bufio"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	mrand "math/rand/v2"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	saltLen = 32
	keyLen  = chacha20poly1305.KeySize
	tagLen  = chacha20poly1305.Overhead

	MaxPayload = 16 * 1024
	MaxPad     = 2 * 1024
	// заголовок кадра: длина полезного (uint16) + длина набивки (uint16)
	lenFieldBytes = 4

	// Буфер чтения намеренно меньше кадра: он нужен лишь чтобы заголовок не
	// стоил отдельного сисколла, а тело io.ReadFull дочитает сам. При десятках
	// тысяч соединений каждый лишний килобайт здесь — это гигабайты памяти.
	readBufSize = 4 * 1024

	infoC2S = "sunfire/1 c2s"
	infoS2C = "sunfire/1 s2c"
)

var ErrUnknownToken = errors.New("sunfire: неизвестный токен")

// framePad — длина набивки кадра. Первый кадр набиваем сильнее, дальше — редко.
func framePad(first bool) int {
	if first {
		return 128 + mrand.IntN(512)
	}
	if mrand.IntN(8) != 0 {
		return 0
	}
	return mrand.IntN(128)
}

func deriveKey(token string, salt []byte, info string) ([]byte, error) {
	r := hkdf.New(sha256.New, []byte(token), salt, []byte(info))
	key := make([]byte, keyLen)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, err
	}
	return key, nil
}

func directionAEADs(token string, salt []byte) (c2s, s2c cipher.AEAD, err error) {
	k1, err := deriveKey(token, salt, infoC2S)
	if err != nil {
		return nil, nil, err
	}
	k2, err := deriveKey(token, salt, infoS2C)
	if err != nil {
		return nil, nil, err
	}
	if c2s, err = chacha20poly1305.New(k1); err != nil {
		return nil, nil, err
	}
	if s2c, err = chacha20poly1305.New(k2); err != nil {
		return nil, nil, err
	}
	return c2s, s2c, nil
}

// Буферы записи переиспользуются между соединениями: в любой момент пишут
// единицы из десятков тысяч сессий, а держать по 36 КиБ на каждую — расточительно.
var wpool = sync.Pool{New: func() any { return new(frameBuf) }}

type frameBuf struct {
	out []byte // кадр целиком
	pad []byte // полезное вместе с набивкой
}

type Conn struct {
	raw       io.ReadWriteCloser
	br        *bufio.Reader // чтение через буфер: заголовок кадра не стоит отдельного сисколла
	enc       cipher.AEAD
	dec       cipher.AEAD
	encNonce  []byte
	decNonce  []byte
	readBuf   []byte
	wroteHead bool
}

func incNonce(nonce []byte) {
	for i := range nonce {
		nonce[i]++
		if nonce[i] != 0 {
			return
		}
	}
}

func (c *Conn) sealFrame(chunk []byte, pad int) error {
	var hdr [lenFieldBytes]byte
	binary.BigEndian.PutUint16(hdr[0:2], uint16(len(chunk)))
	binary.BigEndian.PutUint16(hdr[2:4], uint16(pad))
	fb := wpool.Get().(*frameBuf)
	defer wpool.Put(fb)

	need := lenFieldBytes + tagLen + len(chunk) + pad + tagLen
	if cap(fb.out) < need {
		fb.out = make([]byte, 0, need)
	}
	lenCipher := c.enc.Seal(fb.out[:0], c.encNonce, hdr[:], nil)
	incNonce(c.encNonce)

	var dataCipher []byte
	if pad == 0 {
		dataCipher = c.enc.Seal(lenCipher, c.encNonce, chunk, nil)
	} else {
		if cap(fb.pad) < len(chunk)+pad {
			fb.pad = make([]byte, len(chunk)+pad)
		}
		body := fb.pad[:len(chunk)+pad]
		copy(body, chunk)
		clear(body[len(chunk):]) // набивка нулями, буфер переиспользуется
		dataCipher = c.enc.Seal(lenCipher, c.encNonce, body, nil)
	}
	incNonce(c.encNonce)

	fb.out = dataCipher
	// один Write на кадр: раздельная запись гнала бы на провод отдельный
	// крошечный сегмент перед каждыми 16 КиБ данных
	_, err := c.raw.Write(dataCipher)
	return err
}

func (c *Conn) writeFrame(p []byte) error {
	for len(p) > 0 {
		chunk := p
		if len(chunk) > MaxPayload {
			chunk = chunk[:MaxPayload]
		}
		p = p[len(chunk):]

		first := !c.wroteHead
		c.wroteHead = true
		if err := c.sealFrame(chunk, framePad(first)); err != nil {
			return err
		}
	}
	return nil
}

func (c *Conn) readFrame() error {
	lenCipher := make([]byte, lenFieldBytes+tagLen)
	if _, err := io.ReadFull(c.br, lenCipher); err != nil {
		return err
	}
	hdr, err := c.dec.Open(lenCipher[:0], c.decNonce, lenCipher, nil)
	if err != nil {
		return errors.New("sunfire: повреждённый кадр длины")
	}
	incNonce(c.decNonce)
	payloadLen := int(binary.BigEndian.Uint16(hdr[0:2]))
	padLen := int(binary.BigEndian.Uint16(hdr[2:4]))
	if payloadLen == 0 || payloadLen > MaxPayload || padLen > MaxPad {
		return errors.New("sunfire: недопустимая длина кадра")
	}
	total := payloadLen + padLen
	dataCipher := make([]byte, total+tagLen)
	if _, err := io.ReadFull(c.br, dataCipher); err != nil {
		return err
	}
	plain, err := c.dec.Open(dataCipher[:0], c.decNonce, dataCipher, nil)
	if err != nil {
		return errors.New("sunfire: повреждённый кадр данных")
	}
	incNonce(c.decNonce)
	c.readBuf = plain[:payloadLen]
	return nil
}

func (c *Conn) Read(p []byte) (int, error) {
	if len(c.readBuf) == 0 {
		if err := c.readFrame(); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

func (c *Conn) Write(p []byte) (int, error) {
	if err := c.writeFrame(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *Conn) Close() error { return c.raw.Close() }

func Dial(raw io.ReadWriteCloser, token, targetAddr string) (*Conn, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	c2s, s2c, err := directionAEADs(token, salt)
	if err != nil {
		return nil, err
	}
	if _, err := raw.Write(salt); err != nil {
		return nil, err
	}
	conn := &Conn{
		raw: raw, br: bufio.NewReaderSize(raw, readBufSize), enc: c2s, dec: s2c,
		encNonce: make([]byte, c2s.NonceSize()),
		decNonce: make([]byte, s2c.NonceSize()),
	}
	if err := conn.writeFrame([]byte(targetAddr)); err != nil {
		return nil, err
	}
	return conn, nil
}

// Accept читает соль и первый кадр, подбирает токен, которым кадр расшифровался.
// Дедлайн чтения на raw вызывающий выставляет сам.
func Accept(raw io.ReadWriteCloser, tokens map[string]string) (*Conn, string, string, error) {
	br := bufio.NewReaderSize(raw, readBufSize)
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(br, salt); err != nil {
		return nil, "", "", err
	}
	lenCipher := make([]byte, lenFieldBytes+tagLen)
	if _, err := io.ReadFull(br, lenCipher); err != nil {
		return nil, "", "", err
	}
	for token, user := range tokens {
		k1, err := deriveKey(token, salt, infoC2S)
		if err != nil {
			continue
		}
		c2s, err := chacha20poly1305.New(k1)
		if err != nil {
			continue
		}
		decNonce := make([]byte, c2s.NonceSize())
		hdr, err := c2s.Open(nil, decNonce, lenCipher, nil)
		if err != nil {
			continue
		}
		incNonce(decNonce)
		payloadLen := int(binary.BigEndian.Uint16(hdr[0:2]))
		padLen := int(binary.BigEndian.Uint16(hdr[2:4]))
		if payloadLen == 0 || payloadLen > MaxPayload || padLen > MaxPad {
			continue
		}
		total := payloadLen + padLen
		dataCipher := make([]byte, total+tagLen)
		if _, err := io.ReadFull(br, dataCipher); err != nil {
			return nil, "", "", err
		}
		plain, err := c2s.Open(nil, decNonce, dataCipher, nil)
		if err != nil {
			return nil, "", "", errors.New("sunfire: адрес не расшифровался")
		}
		incNonce(decNonce)
		target := string(plain[:payloadLen])
		k2, err := deriveKey(token, salt, infoS2C)
		if err != nil {
			return nil, "", "", err
		}
		s2c, err := chacha20poly1305.New(k2)
		if err != nil {
			return nil, "", "", err
		}
		conn := &Conn{
			raw: raw, br: br, enc: s2c, dec: c2s,
			encNonce: make([]byte, s2c.NonceSize()),
			decNonce: decNonce,
		}
		return conn, target, user, nil
	}
	return nil, "", "", ErrUnknownToken
}
