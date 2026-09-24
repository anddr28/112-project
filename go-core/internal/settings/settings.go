// Package settings — настройки платформы (таблица settings) в типизированном виде.
//
// Снимок держится в памяти (atomic), перечитывается раз в RefreshEvery (фоном: Run или
// асинхронно из Get) или сразу после Update. Get никогда не возвращает nil и не ходит в БД
// на горячем пути: его зовут внутри открытых транзакций, и синхронный reload через пул
// (второе соединение) под общим мьютексом при насыщенном пуле подвешивал все соединения.
// При недоступной БД — последние известные значения, при первом старте — дефолты из
// миграций. Ключи и значения — snake_case, как в БД (00001/00002).
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RefreshEvery — как часто перечитывать таблицу (правки из другого инстанса / руками в БД).
const RefreshEvery = 30 * time.Second

const (
	// reloadTimeout — потолок фонового перечитывания (ожидание соединения + запрос).
	reloadTimeout = 10 * time.Second
	// retryAfterError — через сколько повторить чтение после ошибки БД (не на каждом Get).
	retryAfterError = 5 * time.Second
	// firstLoadTimeout — потолок синхронной первой загрузки хранилища, которому не вызвали
	// Reload (CLI, тесты). В сервере Build зовёт Reload на старте — этот путь не срабатывает.
	firstLoadTimeout = 3 * time.Second
)

type Weights struct {
	Fields   float64 `json:"fields"`
	Semantic float64 `json:"semantic"`
	Grammar  float64 `json:"grammar"`
	Timing   float64 `json:"timing"`
	Dialogue float64 `json:"dialogue"`
}

func (w Weights) Sum() float64 { return w.Fields + w.Semantic + w.Grammar + w.Timing + w.Dialogue }

// Map — веса по именам слоёв (core.Layer*).
func (w Weights) Map() map[string]float64 {
	return map[string]float64{"fields": w.Fields, "semantic": w.Semantic, "grammar": w.Grammar, "timing": w.Timing, "dialogue": w.Dialogue}
}

type TimingTolerance struct {
	SoftPct float64 `json:"soft_pct"` // превышение норматива без штрафа, %
	HardPct float64 `json:"hard_pct"` // превышение, при котором слой timing = 0, %
}

type XPRules struct {
	AttemptEvaluated     int `json:"attempt_evaluated"`
	WithinNorm           int `json:"within_norm"`
	LessonCompleted      int `json:"lesson_completed"`
	PassBonusPer10Points int `json:"pass_bonus_per_10_points"`
}

type TTS struct {
	Voice      string  `json:"voice"`
	Rate       float64 `json:"rate"`
	SampleRate int     `json:"sample_rate"`
}

// Voice — голосовой режим занятия по умолчанию (lessons.settings.voice).
type Voice struct {
	Enabled    bool   `json:"enabled"`
	Input      string `json:"input"` // voice | text | both
	PushToTalk bool   `json:"push_to_talk"`
	MaxTurns   int    `json:"max_turns"`
	TTSEnabled bool   `json:"tts_enabled"`
}

type STT struct {
	ConfidenceFloor float64 `json:"confidence_floor"`
}

type AI struct {
	ReaperAfterSec   int `json:"reaper_after_sec"`
	MaxTries         int `json:"max_tries"`
	DialogTimeoutSec int `json:"dialog_timeout_sec"`
	BreakerOpenSec   int `json:"breaker_open_sec"`
	BreakerFailures  int `json:"breaker_failures"`
}

type Login struct {
	MaxFailed       int `json:"max_failed"`
	LockMinutes     int `json:"lock_minutes"`
	SessionTTLHours int `json:"session_ttl_hours"`
}

type Backup struct {
	Enabled bool `json:"enabled"`
	Hour    int  `json:"hour"`
	Keep    int  `json:"keep"`
}

// Snapshot — все настройки разом. Неизменяем после публикации — читать без блокировок.
type Snapshot struct {
	TimeLimitSec          int             `json:"time_limit_sec"`
	PassThreshold         float64         `json:"pass_threshold"`
	ScoreWeights          Weights         `json:"score_weights"`
	DialogueWeightDefault float64         `json:"dialogue_weight_default"`
	TimingTolerance       TimingTolerance `json:"timing_tolerance"`
	XPRules               XPRules         `json:"xp_rules"`
	TTS                   TTS             `json:"tts"`
	AuditRetentionDays    int             `json:"audit_retention_days"`
	Voice                 Voice           `json:"voice"`
	STT                   STT             `json:"stt"`
	ConfidenceThreshold   float64         `json:"confidence_threshold"`
	CardsPerStudent       int             `json:"cards_per_student"`
	AllowReplay           bool            `json:"allow_replay"`
	AI                    AI              `json:"ai"`
	Login                 Login           `json:"login"`
	Backup                Backup          `json:"backup"`
	EventsRetentionDays   int             `json:"events_retention_days"`
}

// Defaults — значения сидов 00001/00002 (на случай пустой/недоступной таблицы).
func Defaults() Snapshot {
	return Snapshot{
		TimeLimitSec:          30,
		PassThreshold:         70,
		ScoreWeights:          Weights{Fields: 0.5, Semantic: 0.25, Grammar: 0.1, Timing: 0.15, Dialogue: 0},
		DialogueWeightDefault: 0.25,
		TimingTolerance:       TimingTolerance{SoftPct: 20, HardPct: 100},
		XPRules:               XPRules{AttemptEvaluated: 10, WithinNorm: 5, LessonCompleted: 30, PassBonusPer10Points: 1},
		TTS:                   TTS{Voice: "xenia", Rate: 1.0, SampleRate: 24000},
		AuditRetentionDays:    365,
		Voice:                 Voice{Enabled: false, Input: "voice", PushToTalk: true, MaxTurns: 12, TTSEnabled: true},
		STT:                   STT{ConfidenceFloor: 0.6},
		ConfidenceThreshold:   0.7,
		CardsPerStudent:       1,
		AllowReplay:           true,
		AI:                    AI{ReaperAfterSec: 900, MaxTries: 3, DialogTimeoutSec: 20, BreakerOpenSec: 30, BreakerFailures: 3},
		Login:                 Login{MaxFailed: 5, LockMinutes: 15, SessionTTLHours: 12},
		Backup:                Backup{Enabled: true, Hour: 3, Keep: 14},
		EventsRetentionDays:   365,
	}
}

// Row — строка таблицы settings (для /admin/settings).
type Row struct {
	Key         string
	Value       json.RawMessage
	Description string
	UpdatedAt   time.Time
	UpdatedBy   *uuid.UUID
}

var ErrUnknownKey = errors.New("settings: unknown key")

// Store — кэш настроек поверх таблицы.
type Store struct {
	pool       *pgxpool.Pool
	log        *slog.Logger
	cur        atomic.Pointer[Snapshot]
	loadedAt   atomic.Int64 // unix nano; 0 — ни разу не загружали
	mu         sync.Mutex   // один reload за раз (держат только Reload/Update/первая загрузка, не Get)
	refreshing atomic.Bool  // идёт асинхронное перечитывание (одно на хранилище)
	firstLoad  time.Duration
}

func NewStore(pool *pgxpool.Pool, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{pool: pool, log: log, firstLoad: firstLoadTimeout}
	d := Defaults()
	s.cur.Store(&d)
	return s
}

// Get — текущий снимок. Никогда не блокируется на БД (кроме первой загрузки хранилища,
// которому не вызвали Reload, — она ограничена firstLoadTimeout): устаревший снимок
// отдаётся как есть, перечитывание запускается в фоне (одно на хранилище). Безопасно
// вызывать внутри транзакции при насыщенном пуле.
func (s *Store) Get(ctx context.Context) *Snapshot {
	switch at := s.loadedAt.Load(); {
	case at == 0:
		s.loadFirst(ctx)
	case time.Since(time.Unix(0, at)) > RefreshEvery:
		s.refreshAsync()
	}
	return s.cur.Load()
}

// loadFirst — синхронная первая загрузка (с перепроверкой под мьютексом: ждавшие не
// перечитывают повторно). Ошибка — дефолты, повтор через retryAfterError.
func (s *Store) loadFirst(ctx context.Context) {
	if s.pool == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadedAt.Load() != 0 {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, s.firstLoad)
	defer cancel()
	if err := s.reloadLocked(lctx); err != nil {
		s.log.Warn("settings: first load failed; using defaults", "err", err)
	}
}

// refreshAsync — фоновое перечитывание (не больше одного одновременно). Контекст свой:
// запрос, заметивший устаревание, не ждёт и не отменяет чтение.
func (s *Store) refreshAsync() {
	if s.pool == nil || !s.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.refreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), reloadTimeout)
		defer cancel()
		if err := s.Reload(ctx); err != nil {
			s.log.Warn("settings reload failed; using cached", "err", err)
		}
	}()
}

// Run — перечитывать таблицу каждые every (фоновый воркер сервера; до отмены ctx).
// Get при этом почти никогда не видит устаревший снимок.
func (s *Store) Run(ctx context.Context, every time.Duration) {
	if s.pool == nil || every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, reloadTimeout)
			if err := s.Reload(rctx); err != nil && ctx.Err() == nil {
				s.log.Warn("settings reload failed; using cached", "err", err)
			}
			cancel()
		}
	}
}

// Reload — перечитать таблицу сейчас.
func (s *Store) Reload(ctx context.Context) error {
	if s.pool == nil {
		return errors.New("settings: no database pool")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked(ctx)
}

func (s *Store) reloadLocked(ctx context.Context) error {
	rows, err := s.List(ctx)
	if err != nil {
		// не долбим упавшую БД на каждом запросе, но и не ждём полный RefreshEvery
		s.loadedAt.Store(time.Now().Add(retryAfterError - RefreshEvery).UnixNano())
		return err
	}
	snap := Defaults()
	for _, r := range rows {
		// в копию: json.Unmarshal при ошибке типа успевает заполнить соседние поля —
		// «битое значение игнорируется» должно значить «ключ остался прежним» целиком
		next := snap
		if err := apply(&next, r.Key, r.Value); err != nil {
			s.log.Warn("settings: bad value ignored", "key", r.Key, "err", err)
			continue
		}
		snap = next
	}
	s.cur.Store(&snap)
	s.loadedAt.Store(time.Now().UnixNano())
	return nil
}

// List — все строки (для админки).
func (s *Store) List(ctx context.Context) ([]Row, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, value, COALESCE(description, ''), updated_at, updated_by FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("settings: list: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Key, &r.Value, &r.Description, &r.UpdatedAt, &r.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Update — изменить значение ключа (строгая проверка типа). Возвращает строку до и после
// (для аудита). ErrUnknownKey — ключа нет в схеме настроек.
func (s *Store) Update(ctx context.Context, key string, value json.RawMessage, by uuid.UUID) (before, after Row, err error) {
	probe := Defaults()
	if err := apply(&probe, key, value); err != nil {
		return Row{}, Row{}, err
	}
	if err := validate(&probe, key); err != nil {
		return Row{}, Row{}, err
	}
	err = s.pool.QueryRow(ctx, `SELECT key, value, COALESCE(description, ''), updated_at, updated_by FROM settings WHERE key = $1`, key).
		Scan(&before.Key, &before.Value, &before.Description, &before.UpdatedAt, &before.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		before = Row{Key: key}
	} else if err != nil {
		return Row{}, Row{}, fmt.Errorf("settings: read: %w", err)
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO settings (key, value, updated_by, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now()
		RETURNING key, value, COALESCE(description, ''), updated_at, updated_by`, key, value, by).
		Scan(&after.Key, &after.Value, &after.Description, &after.UpdatedAt, &after.UpdatedBy)
	if err != nil {
		return Row{}, Row{}, fmt.Errorf("settings: update: %w", err)
	}
	_ = s.Reload(ctx)
	return before, after, nil
}

// apply раскладывает значение ключа в снимок. Неизвестный ключ — ErrUnknownKey.
func apply(s *Snapshot, key string, raw json.RawMessage) error {
	var dst any
	switch key {
	case "time_limit_sec":
		dst = &s.TimeLimitSec
	case "pass_threshold":
		dst = &s.PassThreshold
	case "score_weights":
		dst = &s.ScoreWeights
	case "dialogue_weight_default":
		dst = &s.DialogueWeightDefault
	case "timing_tolerance":
		dst = &s.TimingTolerance
	case "xp_rules":
		dst = &s.XPRules
	case "tts":
		dst = &s.TTS
	case "audit_retention_days":
		dst = &s.AuditRetentionDays
	case "voice":
		dst = &s.Voice
	case "stt":
		dst = &s.STT
	case "confidence_threshold":
		dst = &s.ConfidenceThreshold
	case "cards_per_student":
		dst = &s.CardsPerStudent
	case "allow_replay":
		dst = &s.AllowReplay
	case "ai":
		dst = &s.AI
	case "login":
		dst = &s.Login
	case "backup":
		dst = &s.Backup
	case "events_retention_days":
		dst = &s.EventsRetentionDays
	default:
		return ErrUnknownKey
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("значение настройки %q некорректно: %w", key, err)
	}
	return nil
}

// validate — смысловые границы (чтобы админ не сломал оценку опечаткой).
func validate(s *Snapshot, key string) error {
	bad := func(msg string) error { return fmt.Errorf("настройка %q: %s", key, msg) }
	switch key {
	case "time_limit_sec":
		if s.TimeLimitSec < 10 || s.TimeLimitSec > 3600 {
			return bad("норматив должен быть от 10 до 3600 секунд")
		}
	case "pass_threshold":
		if s.PassThreshold < 0 || s.PassThreshold > 100 {
			return bad("порог — от 0 до 100")
		}
	case "score_weights":
		w := s.ScoreWeights
		if w.Fields < 0 || w.Semantic < 0 || w.Grammar < 0 || w.Timing < 0 || w.Dialogue < 0 || w.Sum() <= 0 {
			return bad("веса неотрицательны, сумма больше нуля")
		}
	case "dialogue_weight_default":
		if s.DialogueWeightDefault < 0 || s.DialogueWeightDefault >= 1 {
			return bad("вес разговора — от 0 до 1 (не включая 1)")
		}
	case "confidence_threshold":
		if s.ConfidenceThreshold < 0 || s.ConfidenceThreshold > 1 {
			return bad("порог уверенности — от 0 до 1")
		}
	case "audit_retention_days":
		if s.AuditRetentionDays < 180 {
			return bad("журнал аудита хранится не менее 180 дней (ТЗ)")
		}
	case "voice":
		if s.Voice.MaxTurns < 2 || s.Voice.MaxTurns > 40 {
			return bad("maxTurns — от 2 до 40")
		}
		if s.Voice.Input != "voice" && s.Voice.Input != "text" && s.Voice.Input != "both" {
			return bad("input — voice | text | both")
		}
	case "cards_per_student":
		if s.CardsPerStudent < 1 || s.CardsPerStudent > 50 {
			return bad("от 1 до 50 карточек")
		}
	case "backup":
		if s.Backup.Hour < 0 || s.Backup.Hour > 23 || s.Backup.Keep < 1 {
			return bad("hour 0..23, keep ≥ 1")
		}
	case "login":
		if s.Login.MaxFailed < 1 || s.Login.LockMinutes < 1 || s.Login.SessionTTLHours < 1 {
			return bad("все значения ≥ 1")
		}
	case "ai":
		if s.AI.MaxTries < 1 || s.AI.ReaperAfterSec < 60 || s.AI.BreakerFailures < 1 || s.AI.BreakerOpenSec < 1 ||
			s.AI.DialogTimeoutSec < 0 {
			return bad("max_tries ≥ 1, reaper_after_sec ≥ 60, breaker_* ≥ 1, dialog_timeout_sec ≥ 0")
		}
	case "timing_tolerance":
		if s.TimingTolerance.SoftPct < 0 || s.TimingTolerance.HardPct < s.TimingTolerance.SoftPct {
			return bad("soft_pct ≥ 0, hard_pct ≥ soft_pct")
		}
	case "xp_rules":
		x := s.XPRules
		if x.AttemptEvaluated < 0 || x.WithinNorm < 0 || x.LessonCompleted < 0 || x.PassBonusPer10Points < 0 {
			return bad("начисления XP неотрицательны")
		}
	case "tts":
		if strings.TrimSpace(s.TTS.Voice) == "" || s.TTS.Rate < 0.25 || s.TTS.Rate > 4 || s.TTS.SampleRate <= 0 {
			return bad("voice не пустой, rate от 0.25 до 4, sample_rate > 0")
		}
	case "stt":
		if s.STT.ConfidenceFloor < 0 || s.STT.ConfidenceFloor > 1 {
			return bad("confidence_floor — от 0 до 1")
		}
	case "events_retention_days":
		if s.EventsRetentionDays < 0 {
			return bad("срок хранения неотрицателен (0 — бессрочно)")
		}
	}
	return nil
}
