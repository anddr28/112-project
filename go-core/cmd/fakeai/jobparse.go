package main

import (
	"strings"

	"github.com/google/uuid"

	"lct/gocore/internal/gen/aiservice"
)

// parsedJob — проверенная задача, готовая к постановке в очередь.
type parsedJob struct {
	id       uuid.UUID
	attempt  uuid.UUID
	priority int
	payload  any
}

func trimmed(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// parseJob — разбор и проверка payload по типу задачи: обязательные поля и enum'ы
// контракта ai-service.v1.yaml. Нарушение — 400 (go-core переводит задачу в failed
// без ретраев).
func parseJob(kind jobKind, body []byte, top rawObj) (parsedJob, error) {
	var (
		p    parsedJob
		prio *int
		err  error
	)
	switch kind {
	case kindGrammar:
		if err = requireKeys(top, "", "request_id", "attempt_id", "texts"); err != nil {
			return p, err
		}
		req := new(aiservice.GrammarJobRequest)
		if err = decodeInto(body, req); err != nil {
			return p, err
		}
		if len(req.Texts) == 0 {
			return p, badPayload("texts: нужен хотя бы один текст")
		}
		for i := range req.Texts {
			if strings.TrimSpace(req.Texts[i].Field) == "" {
				return p, badPayload("texts[%d].field пуст", i)
			}
		}
		p = parsedJob{id: req.RequestId, attempt: req.AttemptId, payload: req}
		prio = req.Priority
		if req.AttemptId == uuid.Nil {
			return p, badPayload("attempt_id обязателен")
		}

	case kindSemantic:
		if err = requireKeys(top, "", "request_id", "attempt_id", "mode", "etalon", "answer", "free_text_fields"); err != nil {
			return p, err
		}
		et, err := subObj(top, "etalon", "")
		if err != nil {
			return p, err
		}
		if err = requireKeys(et, "etalon.", "card"); err != nil {
			return p, err
		}
		if _, err = subObj(top, "answer", ""); err != nil {
			return p, err
		}
		if !isNull(top["call_script"]) {
			if err = checkCallScript(top, "call_script"); err != nil {
				return p, err
			}
		}
		req := new(aiservice.SemanticJobRequest)
		if err = decodeInto(body, req); err != nil {
			return p, err
		}
		if !req.Mode.Valid() {
			return p, badPayload("mode: ожидается cards|card_actions")
		}
		if err = checkProfile(req.Profile); err != nil {
			return p, err
		}
		if req.AttemptId == uuid.Nil {
			return p, badPayload("attempt_id обязателен")
		}
		p = parsedJob{id: req.RequestId, attempt: req.AttemptId, payload: req}
		prio = req.Priority

	case kindDialogue:
		if err = requireKeys(top, "", "request_id", "attempt_id", "call_script", "etalon", "transcript"); err != nil {
			return p, err
		}
		if err = checkCallScript(top, "call_script"); err != nil {
			return p, err
		}
		et, err := subObj(top, "etalon", "")
		if err != nil {
			return p, err
		}
		if err = requireKeys(et, "etalon.", "expected_dialogue"); err != nil {
			return p, err
		}
		req := new(aiservice.DialogueJobRequest)
		if err = decodeInto(body, req); err != nil {
			return p, err
		}
		if len(req.Etalon.ExpectedDialogue.Checklist) == 0 {
			return p, badPayload("etalon.expected_dialogue.checklist: нужен хотя бы один пункт")
		}
		for i, it := range req.Etalon.ExpectedDialogue.Checklist {
			if it.Id == "" || !it.Kind.Valid() {
				return p, badPayload("etalon.expected_dialogue.checklist[%d]: нужны id и kind question|instruction|phrase|behavior", i)
			}
		}
		if len(req.Transcript) == 0 {
			return p, badPayload("transcript: нужна хотя бы одна реплика")
		}
		for i, t := range req.Transcript {
			if t.TurnNo < 1 || !t.Speaker.Valid() {
				return p, badPayload("transcript[%d]: нужны turn_no ≥ 1 и speaker operator|caller", i)
			}
			if t.Source != nil && !t.Source.Valid() {
				return p, badPayload("transcript[%d].source: ожидается stt|text|script|llm", i)
			}
		}
		if err = checkProfile(req.Profile); err != nil {
			return p, err
		}
		if req.AttemptId == uuid.Nil {
			return p, badPayload("attempt_id обязателен")
		}
		p = parsedJob{id: req.RequestId, attempt: req.AttemptId, payload: req}
		prio = req.Priority

	case kindGenerate:
		if err = requireKeys(top, "", "request_id", "spec"); err != nil {
			return p, err
		}
		spec, err := subObj(top, "spec", "")
		if err != nil {
			return p, err
		}
		if err = requireKeys(spec, "spec.", "category", "difficulty", "mode"); err != nil {
			return p, err
		}
		req := new(aiservice.GenerateJobRequest)
		if err = decodeInto(body, req); err != nil {
			return p, err
		}
		s := &req.Spec
		switch {
		case strings.TrimSpace(s.Category.Code) == "" || strings.TrimSpace(s.Category.Name) == "":
			return p, badPayload("spec.category: нужны code и name")
		case s.Difficulty < 1 || s.Difficulty > 3:
			return p, badPayload("spec.difficulty: ожидается 1..3")
		case !s.Mode.Valid():
			return p, badPayload("spec.mode: ожидается cards|card_actions|both")
		}
		if err = checkProfile(req.Profile); err != nil {
			return p, err
		}
		p = parsedJob{id: req.RequestId, payload: req}
		prio = req.Priority

	case kindTTS:
		if err = requireKeys(top, "", "request_id", "text", "text_hash"); err != nil {
			return p, err
		}
		req := new(aiservice.TtsJobRequest)
		if err = decodeInto(body, req); err != nil {
			return p, err
		}
		if strings.TrimSpace(req.Text) == "" {
			return p, badPayload("text пуст")
		}
		if !validHash(req.TextHash) {
			return p, badPayload("text_hash: ожидается 8..128 символов [0-9A-Za-z_-]")
		}
		p = parsedJob{id: req.RequestId, payload: req}
		prio = req.Priority
	}

	if p.id == uuid.Nil {
		return p, badPayload("request_id обязателен")
	}
	if p.priority, err = checkPriority(prio); err != nil {
		return p, err
	}
	return p, nil
}
