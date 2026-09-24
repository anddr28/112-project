package app

// Зависимости, которые используют доменные пакеты (закреплены здесь, чтобы `go mod tidy`
// не выкинул их, пока пакеты пишутся параллельно). Добавлять новые — только через лида.
import (
	_ "github.com/coder/websocket"
	_ "github.com/go-pdf/fpdf"
	_ "github.com/google/uuid"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/pressly/goose/v3"
	_ "github.com/xuri/excelize/v2"
	_ "golang.org/x/crypto/argon2"
	_ "golang.org/x/image/font/gofont/gobold"
	_ "golang.org/x/image/font/gofont/goregular"
	_ "golang.org/x/sync/errgroup"
	_ "golang.org/x/sync/singleflight"
)
