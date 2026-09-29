package logring

import (
	"bytes"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func newLogger(ring *Ring, lvl slog.Level) (*slog.Logger, *bytes.Buffer) {
	var out bytes.Buffer
	inner := slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: lvl})
	return slog.New(NewHandler(inner, ring)), &out
}

func TestTeeAndLevels(t *testing.T) {
	t.Parallel()
	ring := New(10)
	log, out := newLogger(ring, slog.LevelWarn)
	log.Debug("отладка")
	log.Info("старт", "addr", ":8443")
	log.Warn("медленно", "dur_ms", 2500)
	log.Error("сбой", "err", errors.New("boom"))

	// stdout — по своему уровню (warn): info туда не попадает
	if s := out.String(); strings.Contains(s, "старт") || !strings.Contains(s, "медленно") || !strings.Contains(s, "сбой") {
		t.Errorf("stdout = %s", s)
	}
	// кольцо — info и выше независимо от уровня stdout, debug не копируется
	got := ring.Snapshot(Query{MinLevel: slog.LevelDebug})
	if len(got) != 3 || got[0].Message != "сбой" || got[2].Message != "старт" {
		t.Fatalf("snapshot = %+v", got)
	}
	if m := got[0].AttrMap(); m["err"] != "boom" {
		t.Errorf("ошибка не зафиксирована строкой: %#v", m)
	}
	if m := got[1].AttrMap(); m["dur_ms"] != int64(2500) {
		t.Errorf("attrs = %#v", m)
	}
	if e := ring.Snapshot(Query{MinLevel: slog.LevelError}); len(e) != 1 || LevelName(e[0].Level) != "error" {
		t.Errorf("фильтр уровня: %+v", e)
	}
}

func TestWithAttrsGroupsAndSearch(t *testing.T) {
	t.Parallel()
	ring := New(10)
	log, _ := newLogger(ring, slog.LevelInfo)
	l := log.With("svc", "go-core").WithGroup("req")
	l.Info("http", "route", "GET /lessons", slog.Group("db", "rows", 3))
	log.Info("другое")

	got := ring.Snapshot(Query{Contains: "LESSONS"})
	if len(got) != 1 {
		t.Fatalf("поиск без учёта регистра по атрибутам: %+v", got)
	}
	m := got[0].AttrMap()
	if m["svc"] != "go-core" || m["req.route"] != "GET /lessons" || m["req.db.rows"] != int64(3) {
		t.Errorf("attrs = %#v", m)
	}
	if got := ring.Snapshot(Query{Contains: "друг"}); len(got) != 1 || got[0].Message != "другое" {
		t.Errorf("поиск по сообщению: %+v", got)
	}
}

func TestRingWrapAndLimit(t *testing.T) {
	t.Parallel()
	ring := New(5)
	log, _ := newLogger(ring, slog.LevelInfo)
	for i := range 12 {
		log.Info("m" + strconv.Itoa(i))
	}
	if ring.Len() != 5 {
		t.Fatalf("len = %d", ring.Len())
	}
	got := ring.Snapshot(Query{})
	want := []string{"m11", "m10", "m9", "m8", "m7"}
	for i, e := range got {
		if e.Message != want[i] {
			t.Fatalf("порядок: %v", got)
		}
	}
	if got := ring.Snapshot(Query{Limit: 2}); len(got) != 2 || got[0].Message != "m11" {
		t.Errorf("limit: %+v", got)
	}
}

func TestConcurrentWriters(t *testing.T) {
	t.Parallel()
	ring := New(100)
	log, _ := newLogger(ring, slog.LevelInfo)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 200 {
				log.Info("w", "g", g, "i", i)
				if i%50 == 0 {
					_ = ring.Snapshot(Query{Contains: "w"})
				}
			}
		})
	}
	wg.Wait()
	if ring.Len() != 100 {
		t.Errorf("len = %d", ring.Len())
	}
}

func TestParseLevel(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]slog.Level{"": slog.LevelInfo, "debug": slog.LevelDebug, "WARN": slog.LevelWarn, "error": slog.LevelError} {
		if l, ok := ParseLevel(in); !ok || l != want {
			t.Errorf("%q -> %v %v", in, l, ok)
		}
	}
	if _, ok := ParseLevel("fatal"); ok {
		t.Error("fatal принят")
	}
}
