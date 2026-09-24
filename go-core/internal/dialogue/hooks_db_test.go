package dialogue

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/platform/pgtest"
)

// core.DialogueHooks (accept-call / submit) и блокировки — поверх настоящей БД.

func acceptCall(t testing.TB, hs *harness, id uuid.UUID) *public.DialogueTurnView {
	t.Helper()
	var v *public.DialogueTurnView
	err := pg.WithTx(context.Background(), hs.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v, err = hs.svc.OnCallAccepted(ctx, tx, id)
		return err
	})
	if err != nil {
		t.Fatalf("OnCallAccepted: %v", err)
	}
	return v
}

func closeOnSubmit(t testing.TB, hs *harness, id uuid.UUID, at time.Time) {
	t.Helper()
	err := pg.WithTx(context.Background(), hs.pool, func(ctx context.Context, tx pgx.Tx) error {
		return hs.svc.CloseOnSubmit(ctx, tx, id, at)
	})
	if err != nil {
		t.Fatalf("CloseOnSubmit: %v", err)
	}
}

func TestOnCallAccepted(t *testing.T) {
	t.Parallel()

	t.Run("голос выключен — nil, ничего не пишется", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{voiceOff: true, noOpening: true})
		if v := acceptCall(t, hs, w.attempt); v != nil {
			t.Fatalf("view: %+v", v)
		}
		if countTurns(t, w.pool, w.attempt) != 0 || len(hs.queue.all()) != 0 {
			t.Fatal("nothing must be written")
		}
	})

	t.Run("вступление без озвучки: задача TTS, повторный accept идемпотентен, озвучка дописывается", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{tts: true, noOpening: true})
		v := acceptCall(t, hs, w.attempt)
		text := "Алло! Пожар! У соседей дым валит!"
		if v == nil || v.TurnNo != 0 || v.Speaker != "caller" || v.Source != "script" || v.Text != text || v.AtMs != 0 ||
			v.Audio != nil || v.DurationMs == nil || *v.DurationMs != speechDurationMs(text) || !v.At.Equal(w.acceptedAt) ||
			v.EmotionalState == nil || *v.EmotionalState != "паника" {
			t.Fatalf("opening: %+v", v)
		}
		jobs := hs.queue.all()
		hash := convert.TTSHash(text, "xenia", 1)
		if len(jobs) != 1 || jobs[0].Type != core.JobTTS || jobs[0].Priority != core.PriorityTTSLive || jobs[0].DedupKey != "tts:"+hash ||
			jobs[0].RefType != core.RefScenario || jobs[0].RefID != w.scenario {
			t.Fatalf("tts job: %+v", jobs)
		}
		p, ok := jobs[0].Payload.(aiservice.TtsJobRequest)
		if !ok || p.Text != text || p.TextHash != hash || p.RequestId != jobs[0].ID || *p.Voice != "xenia" {
			t.Fatalf("payload: %+v", jobs[0].Payload)
		}
		if r := readAttempt(t, w.pool, w.attempt); r.turns != 1 {
			t.Fatalf("dialogue_turns = %d", r.turns)
		}
		if got := hs.pub.monitorTypes(); !reflect.DeepEqual(got, []string{"dialogueTurn"}) {
			t.Fatalf("monitor: %v", got)
		}

		// повтор до озвучки — та же реплика, без второй строки/задачи/публикации
		v2 := acceptCall(t, hs, w.attempt)
		if v2.Text != v.Text || !v2.At.Equal(*v.At) || v2.Audio != nil || countTurns(t, w.pool, w.attempt) != 1 ||
			readAttempt(t, w.pool, w.attempt).turns != 1 || len(hs.queue.all()) != 1 || len(hs.pub.monitorTypes()) != 1 {
			t.Fatalf("repeat: %+v", v2)
		}

		// озвучка доехала без длительности — путь дописан, оценка длительности сохранена
		mustExec(t, w.pool, `INSERT INTO tts_cache (text_hash, voice, file_path) VALUES ($1, 'xenia', $2)`, hash, "ab/"+hash+".wav")
		v3 := acceptCall(t, hs, w.attempt)
		if v3.Audio == nil || v3.Audio.AudioUrl != "/api/v1/media/tts/ab/"+hash+".wav" || v3.DurationMs == nil || *v3.DurationMs != speechDurationMs(text) {
			t.Fatalf("patched: %+v audio=%+v", v3, v3.Audio)
		}
		var path string
		var dur *int
		if err := w.pool.QueryRow(context.Background(), `SELECT audio_path, duration_ms FROM attempt_dialogue_turns WHERE attempt_id = $1 AND turn_no = 0`,
			w.attempt).Scan(&path, &dur); err != nil || path != "ab/"+hash+".wav" || dur == nil || *dur != speechDurationMs(text) {
			t.Fatalf("stored: %q %v %v", path, dur, err)
		}
	})

	t.Run("озвучка уже есть в кэше", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{tts: true, noOpening: true})
		hash := convert.TTSHash("Алло! Пожар! У соседей дым валит!", "xenia", 1)
		mustExec(t, w.pool, `INSERT INTO tts_cache (text_hash, voice, file_path, duration_ms) VALUES ($1, 'xenia', 'cd/x.wav', 2500)`, hash)
		v := acceptCall(t, hs, w.attempt)
		if v.Audio == nil || v.Audio.AudioUrl != "/api/v1/media/tts/cd/x.wav" || *v.DurationMs != 2500 || len(hs.queue.all()) != 0 {
			t.Fatalf("cached: %+v", v)
		}
	})

	t.Run("в легенде нет реплик заявителя — запасная фраза", func(t *testing.T) {
		t.Parallel()
		cs := model.CallScript{Caller: model.Caller{Name: "Аноним"}, Turns: []model.Turn{{Speaker: model.SpeakerOperatorHint, Text: "Назовите адрес"}}}
		hs, w := dbWorld(t, seedOpts{noOpening: true, script: &cs})
		v := acceptCall(t, hs, w.attempt)
		if v.Text != openingFallback || v.EmotionalState != nil {
			t.Fatalf("fallback opening: %+v", v)
		}
	})
}

func TestCloseOnSubmit(t *testing.T) {
	t.Parallel()

	t.Run("открытый разговор закрывается reason=submitted", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{})
		at := time.Now().Add(123456 * time.Nanosecond)
		closeOnSubmit(t, hs, w.attempt, at)
		r := readAttempt(t, w.pool, w.attempt)
		if r.endedAt == nil || !r.endedAt.Equal(eventTime(at)) || r.endReason == nil || *r.endReason != core.EndSubmitted {
			t.Fatalf("attempt: %+v", r)
		}
		if got := eventTypes(t, w.pool, w.attempt); !reflect.DeepEqual(got, []string{"dialogue_ended"}) {
			t.Fatalf("events: %v", got)
		}
		hs.pub.mu.Lock()
		last := hs.pub.monitor[len(hs.pub.monitor)-1]
		hs.pub.mu.Unlock()
		if last.Type != public.MonitorMessageTypeAttemptEvent || last.Event == nil || last.Event.Id == 0 ||
			(*last.Event.Payload)["reason"] != core.EndSubmitted || !last.Event.At.Equal(eventTime(at)) || *last.AttemptId != w.attempt {
			t.Fatalf("ws: %+v", last)
		}

		// повтор — без изменений
		closeOnSubmit(t, hs, w.attempt, time.Now())
		if r2 := readAttempt(t, w.pool, w.attempt); !r2.endedAt.Equal(*r.endedAt) || len(eventTypes(t, w.pool, w.attempt)) != 1 {
			t.Fatalf("repeat changed: %+v", r2)
		}
	})

	t.Run("уже завершён оператором — причина не меняется", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{})
		expectStatus(t, hs.json(t, w.asStudent(), "POST", w.path("/end"), nil), 200)
		closeOnSubmit(t, hs, w.attempt, time.Now())
		if r := readAttempt(t, w.pool, w.attempt); *r.endReason != core.EndOperatorHungUp {
			t.Fatalf("reason: %s", *r.endReason)
		}
	})

	t.Run("голос выключен — ничего", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{voiceOff: true})
		closeOnSubmit(t, hs, w.attempt, time.Now())
		if r := readAttempt(t, w.pool, w.attempt); r.endedAt != nil || len(eventTypes(t, w.pool, w.attempt)) != 0 {
			t.Fatalf("voice off: %+v", r)
		}
	})
}

// Регрессия (ревью: «разговор блокирует попытку FOR UPDATE и тормозит group commit
// событий»): пока транзакция разговора держит замок попытки, вставка события этой попытки
// (FK -> FOR KEY SHARE) не ждёт. С FOR UPDATE вставка упиралась бы в lock_timeout.
func TestDialogueLocksDoNotBlockEventInserts(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	hs := newHarness(t, pool)
	w := newWorld(t, hs, seedOpts{})

	for name, sql := range map[string]string{"sqlLockForTurn": sqlLockForTurn, "sqlStateContextLock": sqlStateContextLock} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck
			rows, err := tx.Query(ctx, sql, w.attempt)
			if err != nil {
				t.Fatal(err)
			}
			rows.Close()
			if rows.Err() != nil {
				t.Fatal(rows.Err())
			}

			other, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Rollback(ctx) //nolint:errcheck
			if _, err := other.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
				t.Fatal(err)
			}
			_, err = other.Exec(ctx, `INSERT INTO attempt_events (attempt_id, client_seq, type, payload, at)
			                          VALUES ($1, 1, 'field_changed', '{}', now())`, w.attempt)
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == pg.CodeLockNotAvailable {
				t.Fatalf("event insert blocked by dialogue lock (%s)", name)
			}
			if err != nil {
				t.Fatal(err)
			}
			// а второй писатель попытки по-прежнему ждёт (замок действительно взят)
			if _, err := other.Exec(ctx, `SAVEPOINT s`); err != nil {
				t.Fatal(err)
			}
			_, err = other.Exec(ctx, `SELECT 1 FROM attempts WHERE id = $1 FOR NO KEY UPDATE`, w.attempt)
			if !errors.As(err, &pgErr) || pgErr.Code != pg.CodeLockNotAvailable {
				t.Fatalf("attempt row must stay locked for writers: %v", err)
			}
		})
	}
}
