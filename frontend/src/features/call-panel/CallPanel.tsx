import { useCallback, useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react';
import { api, hasErrorCode, isApiError } from '../../shared/api';
import { cls } from '../../shared/utils/cls';
import { micStateText, useRecorder, type Recording } from './useRecorder';
import type {
  AttemptEventType, DialogueEndReason, DialogueState, DialogueTurnView, StudentCallScript,
} from '../../shared/types';

/**
 * Панель разговора с заявителем.
 *
 * Две ветки, выбор по настройкам занятия:
 *   — голос выключен: сценарные реплики раскрываются последовательно, как
 *     было до появления интерактивного разговора;
 *   — голос включён: диалог с заявителем через сервисный слой.
 *
 * Обе ветки — отдельные компоненты со своим состоянием: у них разный набор
 * эффектов, и смешивать их в одном теле нельзя.
 */
export function CallPanel({
  attemptId,
  script,
  disabled,
  onReplay,
  onEvent,
}: {
  attemptId: string;
  script: StudentCallScript;
  /** карточка недоступна для правки — разговор тоже закрыт */
  disabled: boolean;
  onReplay: () => void;
  onEvent: (type: AttemptEventType, payload?: Record<string, unknown>) => void;
}) {
  if (script.voice.enabled) {
    return <DialoguePanel attemptId={attemptId} script={script} disabled={disabled} onEvent={onEvent} />;
  }
  return <ScriptedPanel script={script} onReplay={onReplay} />;
}

// ─────────────────────────────────────────────── разговор с ИИ-заявителем

type Phase = 'loading' | 'idle' | 'sending' | 'ended' | 'failed';

interface Notice {
  kind: 'busy' | 'noSpeech' | 'conflict' | 'error';
  text: string;
}

const END_REASON_TEXT: Record<DialogueEndReason, string> = {
  operator_hung_up: 'Вы завершили разговор',
  caller_hung_up: 'Заявитель завершил разговор',
  max_turns: 'Достигнуто максимальное число реплик',
  submitted: 'Разговор завершён вместе с сохранением карточки',
  timeout: 'Разговор прерван по времени',
};

/** Ключ реплики: у оператора и заявителя один номер хода на двоих. */
function turnKey(turn: DialogueTurnView): string {
  return `${turn.speaker}-${turn.turnNo}`;
}

/**
 * Голос заявителя. Штатно это аудио ai-service (`AudioRef` из /media/tts);
 * пока его нет, реплику читает синтезатор браузера — он работает офлайн на
 * голосах ОС. Если русского голоса нет, остаётся текстовая расшифровка.
 */
function speak(text: string): void {
  const synth = typeof window !== 'undefined' ? window.speechSynthesis : undefined;
  if (!synth || typeof SpeechSynthesisUtterance === 'undefined') return;
  const voice = synth.getVoices().find((v) => v.lang.toLowerCase().startsWith('ru'));
  if (!voice) return;
  synth.cancel();
  const u = new SpeechSynthesisUtterance(text);
  u.voice = voice;
  u.lang = voice.lang;
  synth.speak(u);
}

function playCaller(turn: DialogueTurnView, ttsEnabled: boolean): void {
  if (!ttsEnabled) return;
  if (turn.audio?.audioUrl) {
    void new Audio(turn.audio.audioUrl).play().catch(() => speak(turn.text));
    return;
  }
  speak(turn.text);
}

function formatSec(ms: number): string {
  return `${(ms / 1000).toFixed(1).replace('.', ',')} с`;
}

/** Добавляет реплики, не повторяя уже показанные: ответ мог прийти дважды. */
function mergeTurns(current: DialogueTurnView[], incoming: Array<DialogueTurnView | undefined>): DialogueTurnView[] {
  const seen = new Set(current.map(turnKey));
  const added = incoming.filter((t): t is DialogueTurnView => Boolean(t) && !seen.has(turnKey(t as DialogueTurnView)));
  return added.length > 0 ? [...current, ...added] : current;
}

function DialoguePanel({
  attemptId,
  script,
  disabled,
  onEvent,
}: {
  attemptId: string;
  script: StudentCallScript;
  disabled: boolean;
  onEvent: (type: AttemptEventType, payload?: Record<string, unknown>) => void;
}) {
  const [state, setState] = useState<DialogueState | null>(null);
  const [phase, setPhase] = useState<Phase>('loading');
  const [notice, setNotice] = useState<Notice | null>(null);
  const [draft, setDraft] = useState('');
  const [retryIn, setRetryIn] = useState(0);
  const recorder = useRecorder();
  /** Записанная реплика, ожидающая расшифровки или отправки. */
  const [clip, setClip] = useState<Recording | null>(null);
  const micChecked = useRef(false);

  /**
  * Реплика, которую отклонил занятый заявитель: повторяем её тем же номером
  * хода. Это состояние, а не ссылка: от него зависит кнопка повтора.
  */
  const [pendingRetry, setPendingRetry] = useState<{ turnNo: number; text: string; audio: Recording | null } | null>(null);
  const logRef = useRef<HTMLDivElement>(null);
  const voiceInput = script.voice.input !== 'text';
  const ttsEnabled = script.voice.ttsEnabled;

  // Восстановление разговора: и при первом входе, и после перезагрузки
  // страницы — состояние хранит сервисный слой, а не компонент.
  useEffect(() => {
    let cancelled = false;
    api.dialogue
      .get(attemptId)
      .then((next) => {
        if (cancelled) return;
        setState(next);
        setPhase(next.callEnded ? 'ended' : 'idle');
        // Вступительная реплика звучит один раз — пока оператор ещё не отвечал.
        const [first] = next.turns;
        if (!next.callEnded && next.turns.length === 1 && first.speaker === 'caller') {
          playCaller(first, ttsEnabled);
        }
      })
      .catch(() => {
        if (!cancelled) {
          setPhase('failed');
          setNotice({ kind: 'error', text: 'Не удалось загрузить разговор' });
        }
      });
    return () => { cancelled = true; };
  }, [attemptId, ttsEnabled]);

  // Ссылки на записи освобождаем, когда запись больше не показывается.
  useEffect(() => () => {
    if (clip) URL.revokeObjectURL(clip.url);
  }, [clip]);

  useEffect(() => {
    logRef.current?.scrollTo({ top: logRef.current.scrollHeight, behavior: 'smooth' });
  }, [state?.turns.length]);

  // Обратный отсчёт до повтора после отказа «заявитель занят».
  useEffect(() => {
    if (retryIn <= 0) return;
    const id = setTimeout(() => setRetryIn((n) => n - 1), 1000);
    return () => clearTimeout(id);
  }, [retryIn]);

  const refresh = useCallback(async () => {
    const next = await api.dialogue.get(attemptId);
    setState(next);
    setPhase(next.callEnded ? 'ended' : 'idle');
  }, [attemptId]);

  const send = useCallback(
    async (text: string, turnNoOverride?: number, audio: Recording | null = null) => {
      if (!state || state.callEnded || phase === 'sending') return;

      const turnNo = turnNoOverride ?? state.nextTurnNo;
      setPhase('sending');
      setNotice(null);
      setRetryIn(0);

      try {
        const result = await api.dialogue.turn({
          attemptId,
          turnNo,
          text: text || undefined,
          audio: audio?.blob,
          clientRecordedAt: audio?.recordedAt,
        });
        setPendingRetry(null);

        if (result.noSpeech) {
          setNotice({ kind: 'noSpeech', text: 'Речь не распознана. Повторите реплику.' });
          setPhase('idle');
          return;
        }

        setClip(null);
        if (result.caller) playCaller(result.caller, ttsEnabled);

        setState((prev) =>
          prev
            ? {
                ...prev,
                turns: mergeTurns(prev.turns, [result.operator, result.caller]),
                nextTurnNo: result.nextTurnNo,
                callEnded: result.callEnded,
                endReason: result.endReason ?? prev.endReason,
                turnsLeft: prev.turnsLeft == null ? undefined : Math.max(0, prev.turnsLeft - 1),
              }
            : prev,
        );
        setDraft('');
        setPhase(result.callEnded ? 'ended' : 'idle');
      } catch (error) {
        if (hasErrorCode(error, 'caller_busy')) {
          // Номер хода не увеличиваем: повтор пройдёт тем же ходом.
          setPendingRetry({ turnNo, text, audio });
          const wait = (isApiError(error) && error.retryAfterSec) || 3;
          setRetryIn(wait);
          setNotice({ kind: 'busy', text: `Заявитель занят. Повторите попытку через ${wait} с.` });
          setPhase('idle');
          return;
        }

        if (hasErrorCode(error, 'conflict')) {
          setNotice({ kind: 'conflict', text: 'Ход диалога уже обработан. Обновляем разговор…' });
          try {
            await refresh();
            setNotice(null);
          } catch {
            setPhase('failed');
            setNotice({ kind: 'error', text: 'Не удалось обновить разговор' });
          }
          return;
        }

        setNotice({
          kind: 'error',
          text: error instanceof Error ? error.message : 'Не удалось отправить реплику',
        });
        setPhase('idle');
      }
    },
    [attemptId, phase, refresh, state, ttsEnabled],
  );

  const recording = recorder.recording;
  const { start: startRecording, stop: stopRecording } = recorder;

  const startPtt = useCallback(async () => {
    if (recording || disabled || phase !== 'idle' || state?.callEnded) return;
    setNotice(null);
    onEvent('ptt_start');
    const ok = await startRecording();
    if (!micChecked.current) {
      micChecked.current = true;
      onEvent('mic_check', { ok });
    }
  }, [disabled, onEvent, phase, recording, startRecording, state?.callEnded]);

  const stopPtt = useCallback(async () => {
    const result = await stopRecording();
    if (!result) {
      if (recording) {
        onEvent('ptt_stop', { durationMs: 0 });
        setNotice({ kind: 'noSpeech', text: 'Запись слишком короткая. Удерживайте «Говорить», пока говорите.' });
      }
      return;
    }
    onEvent('ptt_stop', { durationMs: result.durationMs, mime: result.mime });
    const text = draft.trim();
    if (text) {
      void send(text, undefined, result);
      return;
    }
    /*
     * Распознавание речи выполняет ai-service. В демо-режиме без него запись
     * отправляется вместе с расшифровкой, которую вводит обучающийся: реплику
     * за него не придумываем, иначе транскрипт и оценка разговора стали бы фикцией.
     */
    setClip(result);
  }, [draft, onEvent, recording, send, stopRecording]);

  async function hangUp() {
    if (!state || state.callEnded) return;
    setPhase('sending');
    try {
      const next = await api.dialogue.end(attemptId);
      setState(next);
      setPhase('ended');
      setNotice(null);
    } catch (error) {
      setNotice({
        kind: 'error',
        text: error instanceof Error ? error.message : 'Не удалось завершить разговор',
      });
      setPhase('idle');
    }
  }

  const ended = Boolean(state?.callEnded);
  const busy = phase === 'sending';
  const composeDisabled = disabled || ended || phase === 'loading' || phase === 'failed';
  const canSend = !composeDisabled && !busy && !recording && draft.trim().length > 0;
  const micProblem = micStateText(recorder.mic);
  const micBlocked = micProblem != null;

  return (
    <div className="arm-panel call-panel">
      <div className="call-panel__head">
        <span>
          Разговор с заявителем
          {script.caller.name ? ` · ${script.caller.name}` : ''}
          {script.caller.role ? `, ${script.caller.role}` : ''}
        </span>
        <span style={{ opacity: 0.85 }}>
          {ended ? 'разговор завершён' : `реплик: ${state?.turns.length ?? 0}`}
          {state?.turnsLeft != null && !ended ? ` · осталось ходов: ${state.turnsLeft}` : ''}
        </span>
      </div>

      <div className="call-panel__log" ref={logRef}>
        {phase === 'loading' && <div className="call-panel__hint">Загрузка разговора…</div>}

        {phase !== 'loading' && (state?.turns.length ?? 0) === 0 && (
          <div className="call-panel__hint">Заявитель на линии. Задайте первый вопрос.</div>
        )}

        {state?.turns.map((turn, i) => (
          <div
            key={turnKey(turn)}
            className={cls(
              'call-turn',
              turn.speaker === 'caller' ? 'call-turn--caller' : 'call-turn--operator',
              i === state.turns.length - 1 && !ended && 'is-current',
            )}
          >
            <div className="call-turn__who">
              {turn.speaker === 'caller' ? 'Заявитель' : 'Оператор'}
              {turn.emotionalState ? ` · ${turn.emotionalState}` : ''}
              {turn.confidence != null && turn.confidence < 0.6 ? ' · распознано неуверенно' : ''}
              {turn.speaker === 'operator' && turn.source === 'stt' ? ' · голосом' : ''}
            </div>
            <div className="call-turn__text">{turn.text}</div>
            {turn.audio?.audioUrl && (
              <audio
                className="call-turn__audio"
                controls
                preload="none"
                src={turn.audio.audioUrl}
                aria-label={turn.speaker === 'caller' ? 'Реплика заявителя' : 'Запись реплики оператора'}
              />
            )}
          </div>
        ))}
      </div>

      <div className="call-panel__status" role="status" aria-live="polite">
        {ended
          ? END_REASON_TEXT[state?.endReason ?? 'operator_hung_up']
          : notice
            ? notice.text
            : busy
              ? 'Заявитель отвечает…'
              : recording
                ? 'Идёт запись…'
                : recorder.mic === 'requesting'
                  ? 'Запрашиваем доступ к микрофону…'
                  : voiceInput && micProblem
                    ? micProblem
                    : clip
                      ? 'Запись сохранена. Введите расшифровку реплики и нажмите «Отправить».'
                      : 'Можно задать вопрос заявителю'}
      </div>

      {!ended && (
        <div className="call-compose">
          <label className="call-compose__field">
            <span className="arm-fld__label">
              {clip ? 'Расшифровка записанной реплики' : 'Реплика оператора'}
            </span>
            <input
              className="arm-line-input"
              value={draft}
              disabled={composeDisabled || busy}
              placeholder={clip ? 'Введите то, что вы сказали' : 'Введите вопрос заявителю'}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={(e) => {
                // Enter отправляет реплику и никогда не кладёт трубку.
                if (e.key === 'Enter' && canSend) {
                  e.preventDefault();
                  void send(draft.trim(), undefined, clip);
                }
              }}
            />
          </label>

          {recording && (
            <div className="call-rec" aria-live="off">
              <span>Запись</span>
              <div className="call-rec__meter" aria-hidden="true">
                <div className="call-rec__level" style={{ width: `${Math.round(recorder.level * 100)}%` }} />
              </div>
              <span className="call-rec__time">{formatSec(recorder.elapsedMs)}</span>
            </div>
          )}

          {clip && !recording && (
            <div className="call-clip">
              <span>Запись {formatSec(clip.durationMs)}</span>
              <audio controls src={clip.url} aria-label="Прослушать свою запись" />
              <button type="button" className="arm-mini" disabled={busy} onClick={() => setClip(null)}>
                Удалить запись
              </button>
            </div>
          )}

          <div className="call-compose__actions">
            {voiceInput && (
              <button
                type="button"
                className={cls('arm-mini', 'call-ptt', recording && 'is-active')}
                disabled={composeDisabled || busy || (micBlocked && !recording)}
                aria-pressed={recording}
                title={micProblem ?? undefined}
                {...(script.voice.pushToTalk
                  ? {
                      onPointerDown: () => void startPtt(),
                      onPointerUp: () => void stopPtt(),
                      onPointerLeave: () => void stopPtt(),
                      onPointerCancel: () => void stopPtt(),
                      onKeyDown: (e: ReactKeyboardEvent) => {
                        if ((e.key === ' ' || e.key === 'Enter') && !e.repeat) {
                          e.preventDefault();
                          void startPtt();
                        }
                      },
                      onKeyUp: (e: ReactKeyboardEvent) => {
                        if (e.key === ' ' || e.key === 'Enter') {
                          e.preventDefault();
                          void stopPtt();
                        }
                      },
                    }
                  : { onClick: () => void (recording ? stopPtt() : startPtt()) })}
              >
                {recording
                  ? script.voice.pushToTalk ? 'Идёт запись…' : 'Остановить запись'
                  : script.voice.pushToTalk ? 'Говорить (удерживать)' : 'Начать запись'}
              </button>
            )}

            <button
              type="button"
              className="arm-mini"
              disabled={!canSend}
              onClick={() => void send(draft.trim(), undefined, clip)}
            >
              {busy ? 'Отправка…' : clip ? 'Отправить запись' : 'Отправить'}
            </button>

            {pendingRetry && notice?.kind === 'busy' && (
              <button
                type="button"
                className="arm-mini"
                disabled={busy || retryIn > 0}
                onClick={() => void send(pendingRetry.text, pendingRetry.turnNo, pendingRetry.audio)}
              >
                {retryIn > 0 ? `Повторить (${retryIn})` : 'Повторить реплику'}
              </button>
            )}

            <button
              type="button"
              className="arm-mini"
              style={{ marginLeft: 'auto' }}
              disabled={composeDisabled || busy}
              onClick={() => void hangUp()}
            >
              Положить трубку
            </button>
          </div>

          <div className="call-compose__hint">
            {voiceInput
              ? 'Запишите реплику голосом или введите её текстом. Распознавание речи выполняет сервер оценки; в демо-режиме к записи прикладывается расшифровка.'
              : 'Реплики вводятся текстом — так настроено занятие.'}
          </div>
        </div>
      )}

      {ended && (
        <div className="call-panel__foot">
          <span style={{ fontSize: 'var(--arm-fs-label)', color: 'var(--arm-label)' }}>
            Разговор закрыт. Карточку можно дозаполнить и сохранить.
          </span>
        </div>
      )}
    </div>
  );
}

// ──────────────────────────────────────────── сценарные реплики (без голоса)

/**
 * Реплики открываются последовательно: текущая подсвечена, предыдущие
 * остаются историей. Кнопка «переспросить» повторяет реплику и увеличивает
 * `replay_count` попытки (учитывается при оценке).
 *
 * TODO(backend) GAP-10 / GAP-14 / GAP-15: контракт `CallScript.turns` даёт
 * только `{speaker, text, ttsHash}` — ни URL аудио, ни длительности.
 * Пока `audioUrl` отсутствует, панель работает как текстовая расшифровка;
 * при появлении раздачи TTS сюда добавляется плеер без изменения разметки.
 */
function ScriptedPanel({ script, onReplay }: { script: StudentCallScript; onReplay: () => void }) {
  const [revealed, setRevealed] = useState(1);
  const [autoPlay, setAutoPlay] = useState(true);
  const logRef = useRef<HTMLDivElement>(null);

  const total = script.turns.length;
  const done = revealed >= total;

  useEffect(() => {
    if (!autoPlay || done) return;
    const current = script.turns[revealed - 1];
    const wait = Math.min(6000, current?.durationMs ?? 3000);
    const id = setTimeout(() => setRevealed((n) => Math.min(total, n + 1)), wait);
    return () => clearTimeout(id);
  }, [autoPlay, revealed, done, total, script.turns]);

  useEffect(() => {
    logRef.current?.scrollTo({ top: logRef.current.scrollHeight, behavior: 'smooth' });
  }, [revealed]);

  const visible = script.turns.slice(0, revealed);

  return (
    <div className="arm-panel call-panel">
      <div className="call-panel__head">
        <span>
          Разговор с заявителем
          {script.caller.name ? ` · ${script.caller.name}` : ''}
          {script.caller.role ? `, ${script.caller.role}` : ''}
        </span>
        <span style={{ opacity: 0.85 }}>{revealed} / {total}</span>
      </div>

      <div className="call-panel__log" ref={logRef}>
        {visible.map((turn, i) => (
          <div
            key={turn.index}
            className={cls(
              'call-turn',
              turn.speaker === 'caller' ? 'call-turn--caller' : 'call-turn--hint',
              i === visible.length - 1 && 'is-current',
            )}
          >
            <div className="call-turn__who">
              {turn.speaker === 'caller' ? 'Заявитель' : 'Подсказка оператору'}
            </div>
            <div className="call-turn__text">{turn.text}</div>
          </div>
        ))}
      </div>

      <div className="call-panel__foot">
        <button
          type="button"
          className="arm-mini"
          disabled={done}
          onClick={() => setRevealed((n) => Math.min(total, n + 1))}
        >
          {done ? 'Обращение завершено' : 'Продолжить'}
        </button>

        {script.allowReplay && (
          <button
            type="button"
            className="arm-mini"
            onClick={() => {
              setRevealed((n) => Math.max(1, n - 1));
              onReplay();
            }}
            title="Попросить заявителя повторить"
          >
            Переспросить
          </button>
        )}

        <button type="button" className="arm-mini" onClick={() => setAutoPlay((v) => !v)}>
          {autoPlay ? 'Пауза' : 'Автовоспроизведение'}
        </button>

        <span style={{ marginLeft: 'auto', fontSize: 'var(--arm-fs-label)', color: 'var(--arm-label)' }}>
          Озвучка подключается отдельно
        </span>
      </div>
    </div>
  );
}
