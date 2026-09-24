package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Параметры argon2id — рекомендация OWASP (m=19 MiB, t=2, p=1): на CPU учебного сервера
// ~20–40 мс на хэш. Хэш хранится в PHC-строке, поэтому параметры можно поднять позже —
// старые хэши проверяются по своим параметрам из строки.
const (
	argonMemoryKiB = 19456
	argonTime      = 2
	argonThreads   = 1
	argonSaltLen   = 16
	argonKeyLen    = 32

	// Верхние границы параметров из строки хэша: испорченная/чужая запись в БД не должна
	// заставить сервер выделить гигабайты памяти.
	maxArgonMemoryKiB = 256 * 1024
	maxArgonTime      = 16
	maxArgonThreads   = 16
	maxArgonKeyLen    = 128

	// MaxPasswordLen — длиннее не хэшируем (защита от «хэшируй мегабайт» и опечаток вставки).
	MaxPasswordLen = 256
)

var (
	// ErrBadHash — строка не argon2id PHC (битая запись в БД).
	ErrBadHash = errors.New("auth: bad password hash format")

	b64 = base64.RawStdEncoding
)

// hashSem ограничивает одновременные вычисления argon2id: каждое держит m=19 MiB, и при
// «шторме входов» (весь класс логинится разом) память растёт линейно. Остальные ждут
// в очереди — вход на пару десятков миллисекунд дольше, зато без всплеска RSS.
var hashSem = make(chan struct{}, max(2, runtime.GOMAXPROCS(0)/2))

func acquireHash(ctx context.Context) error {
	select {
	case hashSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseHash() { <-hashSem }

// HashPassword — argon2id в PHC-формате "$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>".
func HashPassword(pw string) (string, error) {
	return HashPasswordCtx(context.Background(), pw)
}

// HashPasswordCtx — то же с отменой ожидания семафора.
func HashPasswordCtx(ctx context.Context, pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}
	if err := acquireHash(ctx); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	releaseHash()

	var b strings.Builder
	b.Grow(100)
	b.WriteString("$argon2id$v=")
	b.WriteString(strconv.Itoa(argon2.Version))
	b.WriteString("$m=")
	b.WriteString(strconv.Itoa(argonMemoryKiB))
	b.WriteString(",t=")
	b.WriteString(strconv.Itoa(argonTime))
	b.WriteString(",p=")
	b.WriteString(strconv.Itoa(argonThreads))
	b.WriteByte('$')
	b.WriteString(b64.EncodeToString(salt))
	b.WriteByte('$')
	b.WriteString(b64.EncodeToString(key))
	return b.String(), nil
}

// VerifyPassword — сравнение за постоянное время; параметры берутся из строки хэша.
// (false, nil) — пароль не подходит; ошибка — строка хэша испорчена.
func VerifyPassword(encoded, pw string) (bool, error) {
	return VerifyPasswordCtx(context.Background(), encoded, pw)
}

// VerifyPasswordCtx — то же с отменой ожидания семафора.
func VerifyPasswordCtx(ctx context.Context, encoded, pw string) (bool, error) {
	p, err := parsePHC(encoded)
	if err != nil {
		return false, err
	}
	if err := acquireHash(ctx); err != nil {
		return false, err
	}
	key := argon2.IDKey([]byte(pw), p.salt, p.time, p.memory, p.threads, uint32(len(p.key)))
	releaseHash()
	return subtle.ConstantTimeCompare(key, p.key) == 1, nil
}

type phc struct {
	memory  uint32
	time    uint32
	threads uint8
	salt    []byte
	key     []byte
}

// parsePHC разбирает "$argon2id$v=19$m=..,t=..,p=..$salt$hash".
func parsePHC(s string) (phc, error) {
	var p phc
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, ErrBadHash
	}
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return p, ErrBadHash
	}
	var haveM, haveT, haveP bool
	for kv := range strings.SplitSeq(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return p, ErrBadHash
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return p, ErrBadHash
		}
		switch k {
		case "m":
			if n < 8 || n > maxArgonMemoryKiB {
				return p, ErrBadHash
			}
			p.memory, haveM = uint32(n), true
		case "t":
			if n < 1 || n > maxArgonTime {
				return p, ErrBadHash
			}
			p.time, haveT = uint32(n), true
		case "p":
			if n < 1 || n > maxArgonThreads {
				return p, ErrBadHash
			}
			p.threads, haveP = uint8(n), true
		default:
			return p, ErrBadHash
		}
	}
	if !haveM || !haveT || !haveP {
		return p, ErrBadHash
	}
	var err error
	if p.salt, err = b64.DecodeString(parts[4]); err != nil || len(p.salt) < 8 {
		return p, ErrBadHash
	}
	if p.key, err = b64.DecodeString(parts[5]); err != nil || len(p.key) < 16 || len(p.key) > maxArgonKeyLen {
		return p, ErrBadHash
	}
	return p, nil
}

// dummyHash — хэш случайного пароля для выравнивания времени ответа на неизвестный логин:
// без него перебор логинов виден по времени (нет argon2 — ответ мгновенный).
var dummyHash = sync.OnceValue(func() string {
	var raw [24]byte
	_, _ = rand.Read(raw[:])
	h, err := HashPassword(b64.EncodeToString(raw[:]))
	if err != nil {
		panic("auth: dummy hash: " + err.Error())
	}
	return h
})
