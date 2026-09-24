package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// fastHash — PHC-строка argon2id с минимальными параметрами: тестам с БД не нужно тратить
// ~30 мс (под -race — сотни) на каждый вход. Проверка берёт параметры из строки.
func fastHash(t testing.TB, pw string) string {
	t.Helper()
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(pw), salt, 1, 8, 1, 32)
	return "$argon2id$v=19$m=8,t=1,p=1$" + b64.EncodeToString(salt) + "$" + b64.EncodeToString(key)
}

func TestHashVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	for _, pw := range []string{"student", "Пароль-по-русски 123", strings.Repeat("я", 128), " spaces "} {
		h, err := HashPassword(pw)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
			t.Fatalf("формат хэша: %s", h)
		}
		if ok, err := VerifyPassword(h, pw); err != nil || !ok {
			t.Fatalf("%q: верный пароль не принят: %v %v", pw, ok, err)
		}
		if ok, err := VerifyPassword(h, pw+"x"); err != nil || ok {
			t.Fatalf("%q: неверный пароль принят: %v %v", pw, ok, err)
		}
	}
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Fatal("соль не случайна: одинаковые хэши")
	}
}

func TestVerifyFastHash(t *testing.T) {
	t.Parallel()
	h := fastHash(t, "секрет")
	if ok, err := VerifyPassword(h, "секрет"); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ok, _ := VerifyPassword(h, "СЕКРЕТ"); ok {
		t.Fatal("пароль регистрозависим")
	}
}

func TestParsePHCRejectsBadHashes(t *testing.T) {
	t.Parallel()
	good := fastHash(t, "x")
	parts := strings.Split(good, "$")
	salt, key := parts[4], parts[5]
	cases := map[string]string{
		"пусто":             "",
		"мусор":             "!",
		"bcrypt":            "$2a$10$abcdefghijklmnopqrstuv",
		"argon2i":           "$argon2i$v=19$m=8,t=1,p=1$" + salt + "$" + key,
		"версия":            "$argon2id$v=16$m=8,t=1,p=1$" + salt + "$" + key,
		"нет p":             "$argon2id$v=19$m=8,t=1$" + salt + "$" + key,
		"лишний параметр":   "$argon2id$v=19$m=8,t=1,p=1,x=1$" + salt + "$" + key,
		"не число":          "$argon2id$v=19$m=eight,t=1,p=1$" + salt + "$" + key,
		"без =":             "$argon2id$v=19$m8,t=1,p=1$" + salt + "$" + key,
		"память мала":       "$argon2id$v=19$m=7,t=1,p=1$" + salt + "$" + key,
		"память огромна":    "$argon2id$v=19$m=4194304,t=1,p=1$" + salt + "$" + key,
		"t=0":               "$argon2id$v=19$m=8,t=0,p=1$" + salt + "$" + key,
		"t велико":          "$argon2id$v=19$m=8,t=17,p=1$" + salt + "$" + key,
		"p=0":               "$argon2id$v=19$m=8,t=1,p=0$" + salt + "$" + key,
		"p велико":          "$argon2id$v=19$m=8,t=1,p=17$" + salt + "$" + key,
		"соль не base64":    "$argon2id$v=19$m=8,t=1,p=1$!!!$" + key,
		"соль коротка":      "$argon2id$v=19$m=8,t=1,p=1$" + b64.EncodeToString([]byte("1234567")) + "$" + key,
		"ключ короток":      "$argon2id$v=19$m=8,t=1,p=1$" + salt + "$" + b64.EncodeToString(make([]byte, 15)),
		"ключ огромен":      "$argon2id$v=19$m=8,t=1,p=1$" + salt + "$" + b64.EncodeToString(make([]byte, 129)),
		"лишний сегмент":    good + "$x",
		"без ведущего $":    strings.TrimPrefix(good, "$"),
		"отрицательное m":   "$argon2id$v=19$m=-8,t=1,p=1$" + salt + "$" + key,
		"переполнение uint": "$argon2id$v=19$m=99999999999,t=1,p=1$" + salt + "$" + key,
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ok, err := VerifyPassword(h, "x")
			if !errors.Is(err, ErrBadHash) || ok {
				t.Fatalf("ok=%v err=%v, want ErrBadHash", ok, err)
			}
		})
	}
}

func TestDummyHashIsValid(t *testing.T) {
	t.Parallel()
	h := dummyHash()
	if h != dummyHash() {
		t.Fatal("пустышка пересчитывается")
	}
	if _, err := parsePHC(h); err != nil {
		t.Fatalf("пустышка не парсится: %v", err)
	}
	if ok, _ := VerifyPassword(h, ""); ok {
		t.Fatal("пустышка принимает пустой пароль")
	}
}

// Отмена контекста, пока все слоты argon2 заняты, — ошибка контекста, а не зависание.
// Не параллельный: занимает глобальный семафор (параллельные тесты пакета в это время стоят).
func TestHashSemaphoreHonoursContext(t *testing.T) {
	for range cap(hashSem) {
		hashSem <- struct{}{}
	}
	defer func() {
		for range cap(hashSem) {
			<-hashSem
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VerifyPasswordCtx(ctx, fastHash(t, "x"), "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("verify: err=%v, want context.Canceled", err)
	}
	if _, err := HashPasswordCtx(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("hash: err=%v, want context.Canceled", err)
	}
}
