package core

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func TestShortName(t *testing.T) {
	t.Parallel()
	cases := []struct{ last, first, middle, want string }{
		{"Иванов", "Пётр", "Сергеевич", "Иванов П. С."},
		{"Иванов", "Пётр", "", "Иванов П."},
		{"  Иванова ", " Анна ", "  ", "Иванова А."},
		{"Ёлкин", "Ёжик", "Эдуардович", "Ёлкин Ё. Э."},
		{"Smith", "John", "", "Smith J."},
		{"", "", "", ""},
		{"Оглы", "", "Мамедович", "Оглы М."},
	}
	for _, c := range cases {
		if got := ShortName(c.last, c.first, c.middle); got != c.want {
			t.Errorf("ShortName(%q,%q,%q) = %q, want %q", c.last, c.first, c.middle, got, c.want)
		}
	}
}

func TestPrincipal(t *testing.T) {
	t.Parallel()
	p := &Principal{Role: RoleTeacher, LastName: "Петрова", FirstName: "Мария", MiddleName: "Ивановна"}
	if p.ShortName() != "Петрова М. И." || p.OperatorLabel() != "Петрова М. И." {
		t.Fatalf("%q %q", p.ShortName(), p.OperatorLabel())
	}
	p.OperatorNo = "оп. 227"
	if p.OperatorLabel() != "оп. 227" {
		t.Fatal("OperatorLabel с номером")
	}
	p.OperatorNo = "   "
	if p.OperatorLabel() != "Петрова М. И." {
		t.Fatal("пробельный номер — не номер")
	}
	if !p.Is(RoleAdmin, RoleTeacher) || p.Is(RoleStudent) || p.Is() {
		t.Fatal("Is")
	}
	for r, ok := range map[Role]bool{RoleAdmin: true, RoleTeacher: true, RoleStudent: true, "": false, "Admin": false, "root": false} {
		if r.Valid() != ok {
			t.Errorf("Role(%q).Valid() = %v", r, !ok)
		}
	}
}

func TestContextHelpers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if PrincipalFrom(ctx) != nil {
		t.Fatal("principal в пустом контексте")
	}
	if _, ok := RequestMetaFrom(ctx); ok {
		t.Fatal("meta в пустом контексте")
	}
	p := &Principal{UserID: uuid.New(), Role: RoleStudent}
	m := RequestMeta{RequestID: uuid.New(), IP: "10.0.0.1", UserAgent: "test"}
	ctx = WithRequestMeta(WithPrincipal(ctx, p), m)
	if PrincipalFrom(ctx) != p {
		t.Fatal("PrincipalFrom")
	}
	if got, ok := RequestMetaFrom(ctx); !ok || got != m {
		t.Fatal("RequestMetaFrom")
	}
}

func TestAttemptTerminal(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		AttemptIssued: false, AttemptInProgress: false,
		AttemptSubmitted: true, AttemptEvaluating: true, AttemptEvaluated: true, AttemptExpired: true, AttemptAborted: true,
		"": false, "unknown": false,
	} {
		if AttemptTerminal(s) != want {
			t.Errorf("AttemptTerminal(%q) = %v", s, !want)
		}
	}
}

func TestUserInputEvent(t *testing.T) {
	t.Parallel()
	input := []string{EventFieldChanged, EventChooseValue, EventServiceAssigned, EventServiceRemoved, EventServiceStatusChanged}
	notInput := []string{EventIssued, EventCallAccepted, EventOpenCard, EventSave, EventReplay, EventSubmitted,
		EventTimerExpired, EventDisconnected, EventReconnected, EventMicCheck, EventPTTStart, EventPTTStop,
		EventDialogueOperator, EventDialogueCaller, EventDialogueEnded, ""}
	for _, e := range input {
		if !UserInputEvent(e) {
			t.Errorf("%s — ввод", e)
		}
	}
	for _, e := range notInput {
		if UserInputEvent(e) {
			t.Errorf("%s — не ввод (время реакции не должно от него считаться)", e)
		}
	}
}

func TestJobType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		t           JobType
		path, layer string
	}{
		{JobEvaluateGrammar, "/v1/jobs/grammar", LayerGrammar},
		{JobEvaluateSemantic, "/v1/jobs/semantic", LayerSemantic},
		{JobEvaluateDialogue, "/v1/jobs/dialogue", LayerDialogue},
		{JobGenerateScenario, "/v1/jobs/generate", ""},
		{JobTTS, "/v1/jobs/tts", ""},
		{"bogus", "", ""},
	}
	for _, c := range cases {
		if c.t.Path() != c.path || c.t.Layer() != c.layer {
			t.Errorf("%s: path %q layer %q", c.t, c.t.Path(), c.t.Layer())
		}
	}
}

func TestAIError(t *testing.T) {
	t.Parallel()
	cause := errors.New("connection refused")
	e := &AIError{Kind: AIUnavailable, Message: "недоступен", Err: cause}
	if e.Error() != "ai-service: недоступен: connection refused" || !errors.Is(e, cause) {
		t.Fatalf("%q", e.Error())
	}
	if (&AIError{Kind: AIBusy, Message: "занят"}).Error() != "ai-service: занят" {
		t.Fatal("Error без причины")
	}
	got, ok := AsAIError(fmt.Errorf("dialogue: %w", e))
	if !ok || got != e || got.Kind != AIUnavailable {
		t.Fatal("AsAIError обёрнутой")
	}
	if _, ok := AsAIError(cause); ok {
		t.Fatal("AsAIError чужой")
	}
	if _, ok := AsAIError(nil); ok {
		t.Fatal("AsAIError(nil)")
	}
}
