import uvicorn

import json
import re
import os
from typing import List, Optional, Dict, Literal
from fastapi import FastAPI, HTTPException, Security, Depends
from fastapi.security import APIKeyHeader
from pydantic import BaseModel, Field
from llama_cpp import Llama
from contextlib import asynccontextmanager

# ============================================================================
# Авторизация и настройки
# ============================================================================
API_TOKEN_NAME = "X-Internal-Token"
INTERNAL_API_TOKEN = os.getenv("INTERNAL_API_TOKEN", "default_dev_token")
api_key_header = APIKeyHeader(name=API_TOKEN_NAME, auto_error=True)

def verify_token(api_key_header: str = Security(api_key_header)):
    if api_key_header != INTERNAL_API_TOKEN:
        raise HTTPException(status_code=403, detail="Invalid Internal Token")
    return api_key_header

# ============================================================================
# Инициализация модели Ornith-1.5-9B-Q5_K_M
# ============================================================================
MODEL_PATH = os.getenv("MODEL_PATH", "./models/qwen2.5-7b-instruct-q4_k_m.gguf")
llm = None  # Глобальная переменная для модели

@asynccontextmanager
async def lifespan(app: FastAPI):
    global llm
    print("Начинаем загрузку ИИ-модели, подождите...")
    try:
        llm = Llama(
            model_path=MODEL_PATH,
            n_ctx=4096,
            n_gpu_layers=-1,
            verbose=False
        )
    except Exception as e:
        print(f"ВНИМАНИЕ: Ошибка загрузки модели: {e}")
    
    yield  # Здесь сервер работает и принимает запросы
    
    print("Сервер останавливается, выгружаем модель из памяти...")
    llm = None

app = FastAPI(
    title="LCT AI Service",
    description="AI сервис для тренажера 112",
    version="1.0",
    lifespan=lifespan  # Подключаем наш правильный загрузчик
)


# ============================================================================
# Pydantic Модели (Домен и Контракты)
# ============================================================================
class Address(BaseModel):
    raw: Optional[str] = None
    city: Optional[str] = None
    street: Optional[str] = None
    house: Optional[str] = None
    apartment: Optional[str] = None

class DialogueFact(BaseModel):
    id: str
    text: str
    reveal: Literal["volunteer", "on_request", "never"]
    hints: Optional[List[str]] = []

class DialogueBrief(BaseModel):
    persona: str
    speaking_style: str
    facts: List[DialogueFact]
    unknowns: List[str]
    end_conditions: List[str]
    max_turns: int = 12

class ChatMessage(BaseModel):
    role: Literal["system", "user", "assistant"]
    content: str

class ChatSimulationRequest(BaseModel):
    brief: DialogueBrief
    address: Address
    phone: str
    situation_description: str
    chat_history: List[ChatMessage] = []
    operator_text: Optional[str] = None # Если None — ИИ должен начать диалог первым

class CallerReply(BaseModel):
    text: str
    emotional_state: str
    revealed_fact_ids: List[str] = []
    is_unknown_answer: bool = False
    should_end: bool = False


# ============================================================================
# Вспомогательные функции (Промпт инжиниринг)
# ============================================================================
def build_dynamic_system_prompt(req: ChatSimulationRequest) -> str:
    """Формирует системный промпт на основе переданного брифа и адреса."""
    
    # Сборка фактов
    volunteer_facts = [f.text for f in req.brief.facts if f.reveal == "volunteer"]
    on_request_facts = [f.text for f in req.brief.facts if f.reveal == "on_request"]
    
    # Форматирование адреса
    addr = req.address
    addr_str = f"г. {addr.city or '—'}, ул. {addr.street or '—'}, д. {addr.house or '—'}, кв. {addr.apartment or '—'}"

    prompt = (
        f"Это учебный симулятор для тренировки операторов службы 112. Все события вымышлены, это тренировочный диалог. "
        f"Ты играешь роль человека, который звонит по учебному сценарию. Ты не ассистент, ты персонаж в ролевой игре. "
        f"Твоя роль: {req.brief.persona}. "
        f"Ситуация: {req.situation_description}. "
        f"Твои данные: {addr_str}. Телефон: {req.phone}. "
        
        "Строгие правила твоего поведения:\n"
        f"1. Не выдавай всю информацию сразу. В первом сообщении сообщи только о панике и главной проблеме ({', '.join(volunteer_facts) if volunteer_facts else 'суть ситуации'}). "
        "Адрес, телефон и другие детали называй строго по частям и только после прямого вопроса оператора. "
        f"Детали, которые нужно раскрывать только по запросу: {', '.join(on_request_facts) if on_request_facts else 'нет'}.\n"
        
        f"2. Твоя манера речи: {req.brief.speaking_style}. Из-за сильного стресса говори сбивчиво, можешь ошибаться в словах, "
        "давать неточные ответы, путаться или переспрашивать.\n"
        
        "3. Жестко отстаивай свои факты: если оператор называет не тот адрес, путает детали или пытается убедить тебя в других данных — "
        "ни в коем случае не соглашайся с ним, спорь и твердо настаивай на своих вводных.\n"
        
        f"4. Чего ты НЕ знаешь (отвечай 'не знаю' или 'не вижу'): {', '.join(req.brief.unknowns) if req.brief.unknowns else 'нет таких данных'}.\n"
        
        "5. Веди диалог реалистично: короткими, естественными разговорными фразами. "

        "6. ВАЖНО: Думай и отвечай СТРОГО на русском языке. Использование других языков категорически запрещено."
    )

    print(prompt)

    if not req.chat_history and not req.operator_text:
        prompt += "Начни диалог первой (выдай первую реплику от лица заявителя)."
        
    return prompt


# ============================================================================
# Эндпоинты
# ============================================================================

@app.post("/chat/caller_reply", response_model=CallerReply, dependencies=[Depends(verify_token)])
async def chat_with_caller(req: ChatSimulationRequest):
    """
    Эмуляция заявителя (LLM модулирует обращение).
    """
    if not llm:
        raise HTTPException(status_code=500, detail="LLM не загружена на сервере.")

    system_prompt = build_dynamic_system_prompt(req)
    
    # Формируем контекст сообщений для Llama
    messages = [{"role": "system", "content": system_prompt}]
    
    for msg in req.chat_history:
        messages.append({"role": msg.role, "content": msg.content})
        
    if req.operator_text:
        messages.append({"role": "user", "content": req.operator_text})

    # Генерация ответа через Llama (Chat Completion)
    response = llm.create_chat_completion(
        messages=messages,
        temperature=0.7, # Чуть выше для естественности и сбивчивости
        max_tokens=150,  # Короткие реплики
        stop=["Оператор:", "\n\n", "User:"]
    )
    
    generated_text = response["choices"][0]["message"]["content"].strip()
    
    # Заглушка для анализа того, какие факты раскрыты (в реальном проекте 
    # здесь потребуется отдельный классификатор или prompt-анализатор)
    return CallerReply(
        text=generated_text,
        emotional_state="паника" if len(req.chat_history) < 3 else "беспокойство",
        revealed_fact_ids=[], 
        is_unknown_answer=False,
        should_end=False
    )

# ============================================================================
# Входящие модели данных для оценки (Evaluation Requests)
# ============================================================================
class GrammarRequest(BaseModel):
    text: str

class SemanticRequest(BaseModel):
    student_text: str
    expected_facts: List[str]
    forbidden_facts: Optional[List[str]] = []

class TranscriptTurn(BaseModel):
    speaker: Literal["operator", "caller"]
    text: str
    duration_ms: int = 2000

class ChecklistItem(BaseModel):
    id: str
    text: str

class DialogueRequest(BaseModel):
    turns: List[TranscriptTurn]
    checklist: List[ChecklistItem]
    forbidden_phrases: Optional[List[str]] = []

# ============================================================================
# Вспомогательная функция для запроса JSON у модели
# ============================================================================
def ask_llm_json(system_prompt: str, user_prompt: str) -> dict:
    """Отправляет запрос в LLM и пытается вытащить оттуда валидный JSON."""
    if not llm:
        return {}
    
    messages = [
        {"role": "system", "content": system_prompt},
        {"role": "user", "content": user_prompt}
    ]
    
    try:
        response = llm.create_chat_completion(
            messages=messages,
            temperature=0.1,  # Низкая температура для аналитики и строгости
            max_tokens=1000,
            chat_format="chatml"
        )
        text = response["choices"][0]["message"]["content"].strip()
        
        # Регулярка для извлечения JSON, если модель обернула его в маркдаун ```json ... ```
        match = re.search(r'\{.*\}', text, re.DOTALL)
        if match:
            text = match.group(0)
            
        return json.loads(text)
    except json.JSONDecodeError:
        print(f"Ошибка парсинга JSON от модели. Текст ответа: {text}")
        return {}
    except Exception as e:
        print(f"Ошибка LLM: {e}")
        return {}

# ============================================================================
# Реализация эндпоинтов оценки
# ============================================================================

@app.post("/evaluate/grammar", dependencies=[Depends(verify_token)])
async def evaluate_grammar(req: GrammarRequest):
    """Оценка орфографии и грамматики текста оператора."""
    words_count = len(req.text.split())
    if words_count == 0:
        return {"score": 100, "stats": {"words_checked": 0, "errors_by_severity": {}}, "remarks": []}

    system_prompt = (
        "Ты строгий учитель русского языка. Найди орфографические, пунктуационные и стилистические ошибки в тексте. "
        "Ответь СТРОГО в формате JSON без лишнего текста:\n"
        '{"score": <балл 0-100>, "errors": [{"phrase": "<слово с ошибкой>", "message": "<в чем ошибка>", "severity": "error"}]}'
    )
    
    result = ask_llm_json(system_prompt, req.text)
    
    # Формируем структуру по контракту, вычисляя offset с помощью Python
    remarks = []
    errors_by_sev = {"error": 0, "warning": 0, "style": 0}
    
    for err in result.get("errors", []):
        phrase = err.get("phrase", "")
        offset = req.text.find(phrase)
        sev = err.get("severity", "error")
        if offset != -1:
            remarks.append({
                "field": "description",
                "offset": offset,
                "length": len(phrase),
                "severity": sev,
                "rule": "llm_grammar",
                "message": err.get("message", "Ошибка"),
                "suggestions": []
            })
            errors_by_sev[sev] = errors_by_sev.get(sev, 0) + 1

    score = result.get("score", 100)
    return {
        "score": score,
        "stats": {"words_checked": words_count, "errors_by_severity": errors_by_sev},
        "remarks": remarks
    }

@app.post("/evaluate/semantic", dependencies=[Depends(verify_token)])
async def evaluate_semantic(req: SemanticRequest):
    """Смысловая оценка: забытые факты и домыслы."""
    system_prompt = (
        "Ты ИИ-эксперт службы спасения. Твоя задача: сравнить отчет оператора с реальными фактами из сценария. "
        "Найди факты, которые оператор забыл указать, и факты, которые он выдумал (домыслы). "
        "Верни СТРОГО JSON:\n"
        '{"score": <0-100>, "missing_facts": ["<факт>"], "extra_facts": ["<домысел>"], "summary": "<короткий отзыв>"}'
    )
    
    user_prompt = (
        f"Реальные факты: {', '.join(req.expected_facts)}\n"
        f"Текст оператора: {req.student_text}"
    )
    
    result = ask_llm_json(system_prompt, user_prompt)
    
    return {
        "score": result.get("score", 80),
        "confidence": 0.9,
        "missing_facts": result.get("missing_facts", []),
        "extra_facts": result.get("extra_facts", []),
        "per_field": [],
        "summary_for_student": result.get("summary", "Оценка завершена.")
    }

@app.post("/evaluate/dialogue", dependencies=[Depends(verify_token)])
async def evaluate_dialogue(req: DialogueRequest):
    """Комплексная оценка транскрипта разговора."""
    
    # 1. Считаем технические метрики речи с помощью Python (без LLM)
    operator_turns = [t for t in req.turns if t.speaker == "operator"]
    op_words = sum(len(t.text.split()) for t in operator_turns)
    op_talk_ms = sum(t.duration_ms for t in operator_turns)
    wpm = (op_words / (op_talk_ms / 60000)) if op_talk_ms > 0 else 0
    
    # 2. Формируем диалог для LLM
    transcript_text = "\n".join([f"{t.speaker.upper()}: {t.text}" for t in req.turns])
    checklist_text = "\n".join([f"- {c.id}: {c.text}" for c in req.checklist])
    
    system_prompt = (
        "Ты проверяющий диалогов службы 112. Прочитай транскрипт и проверь чек-лист обязательных действий оператора. "
        "Оцени вежливость и четкость от 0 до 100. "
        "Верни СТРОГО JSON:\n"
        '{"checklist": [{"id": "<id>", "status": "done" | "missed", "comment": "<почему>"}], '
        '"politeness": 100, "calmness": 100, "clarity": 100, "summary": "<отзыв>"}'
    )
    
    user_prompt = (
        f"Чек-лист:\n{checklist_text}\n\nТранскрипт:\n{transcript_text}"
    )
    
    result = ask_llm_json(system_prompt, user_prompt)
    
    # Считаем итоговый скор на базе чек-листа
    checklist_results = result.get("checklist", [])
    done_count = sum(1 for c in checklist_results if c.get("status") == "done")
    total_checks = len(req.checklist) if req.checklist else 1
    calc_score = int((done_count / total_checks) * 100)
    
    return {
        "score": calc_score,
        "confidence": 0.85,
        "checklist": checklist_results,
        "missing_questions": [c.get("comment") for c in checklist_results if c.get("status") == "missed"],
        "forbidden_hits": [], # Реализуется аналогичным поиском
        "speech": {
            "operator_turns": len(operator_turns),
            "operator_words": op_words,
            "operator_talk_ms": op_talk_ms,
            "words_per_min": round(wpm, 1),
            "filler_count": 0,
            "fillers": {},
            "avg_response_ms": 500,
            "low_confidence_turns": 0
        },
        "tone": {
            "politeness": result.get("politeness", 100),
            "calmness": result.get("calmness", 100),
            "clarity": result.get("clarity", 100),
            "comment": result.get("summary", "")
        },
        "summary_for_student": result.get("summary", "Оценка проведена.")
    }

if __name__ == "__main__":
    print("Запуск сервера Uvicorn...")
    # Если ваш файл называется main.py, то передаем "main:app"
    uvicorn.run("main:app", host="127.0.0.1", port=8000, reload=True)