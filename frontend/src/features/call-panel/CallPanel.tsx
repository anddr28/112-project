import { useCallback, useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react';
import { api, hasErrorCode, isApiError } from '../../shared/api';
import { cls } from '../../shared/utils/cls';
import { MAX_RECORDING_MS, micStateText, useRecorder, type Recording } from './useRecorder';
import {
  speechFailureText, speechSupported, useSpeechRecognition, type SpeechFailure,
} from './useSpeechRecognition';
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
  kind: 'busy' | 'noSpeech' | 'conflict' | 'error' | 'info';
  text: string;
}

const END_REASON_TEXT: Record<DialogueEndReason, string> = {
  operator_hung_up: 'Вы завершили разговор',
  caller_hung_up: 'Заявитель завершил разговор',
  max_turns: 'Достигнуто максимальное число реплик',
  submitted: 'Разговор завершён вместе с сохранением карточки',
  timeout: 'Разговор прерван по времени',
};

const STT_STATUS: Record<SttState, string> = {
  idle: 'Аудиозапись сохранена.',
  pending: 'Аудиозапись сохранена. Браузер распознаёт речь…',
  ready: 'Речь распознана. Проверьте текст расшифровки и нажмите «Отправить».',
  manual: 'Аудиозапись сохранена. Автоматическая расшифровка недоступна — введите текст реплики.',
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

/**
 * Расшифровка записанной реплики до отправки:
 *   pending — браузер распознаёт речь; ready — распознанный текст в поле;
 *   manual — распознавания нет, расшифровку вводит обучающийся.
 *
 * Заготовленные реплики сюда не попадают никогда: в поле либо то, что
 * распознал браузер, либо то, что ввёл сам обучающийся.
 */
type SttState = 'idle' | 'pending' | 'ready' | 'manual';

/**
 * Граница между нажатием и удержанием.
 *
 * Отпустили раньше — это обычное нажатие, запись продолжается до второго
 * нажатия («Остановить запись»). Держали дольше — рация: запись кончается
 * вместе с удержанием.
 */
const HOLD_MS = 500;

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
  /** Записанная реплика, ожидающая расшифровки или отправки. */
  const [clip, setClip] = useState<Recording | null>(null);
  const [stt, setStt] = useState<SttState>('idle');
  /** Почему расшифровки нет: показываем причину, а не выдуманный текст. */
  const [sttReason, setSttReason] = useState<SpeechFailure | null>(null);
  /** Распознано на устройстве — звук наружу не отправлялся. */
  const [sttLocal, setSttLocal] = useState(false);
  /** Номер текущего распознавания: поздний ответ по старой записи игнорируется. */
  const sttRun = useRef(0);
  const micChecked = useRef(false);
  /** Расшифровка объявлена ниже: авто-стоп обращается к ней через ref. */
  const transcribeRef = useRef<((rec: Recording) => Promise<void>) | null>(null);
  /** Момент нажатия на кнопку записи: по нему отличаем удержание от нажатия. */
  const pressedAt = useRef(0);
  /** Микрофон ещё выдаётся: отпускание кнопки не должно отменять старт. */
  const [starting, setStarting] = useState(false);

  /** Запись остановил лимит длительности — обрабатываем как обычное завершение. */
  const handleAutoStop = useCallback((result: Recording | null) => {
    if (!result) return;
    onEvent('ptt_stop', { durationMs: result.durationMs, mime: result.mime, reason: 'limit' });
    setNotice({
      kind: 'noSpeech',
      text: `Достигнут предел длительности записи (${Math.round(MAX_RECORDING_MS / 1000)} с). Запись сохранена.`,
    });
    setClip(result);
    void transcribeRef.current?.(result);
  }, [onEvent]);

  const recorder = useRecorder({ onAutoStop: handleAutoStop });
  const speech = useSpeechRecognition();

  /**
  * Реплика, которую отклонил занятый заявитель: повторяем её тем же номером
  * хода. Это состояние, а не ссылка: от него зависит кнопка повтора.
  */
  const [pendingRetry, setPendingRetry] = useState<{ turnNo: number; text: string; audio: Recording | null } | null>(null);
  const logRef = useRef<HTMLDivElement>(null);
  const voiceInput = script.voice.input !== 'text';
  // Распознавание речи есть не во всяком браузере и не во всякой сети.
  const speechAvailable = speechSupported();
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

        sttRun.current += 1;
        setClip(null);
        setStt('idle');
        setSttReason(null);
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
        // Сервис ИИ недоступен: сервер ответил сценарной репликой — говорим об этом прямо.
        if (result.fallback) {
          setNotice({ kind: 'info', text: 'Сервис ИИ недоступен — заявитель отвечает репликами из сценария.' });
        }
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

  /**
   * Расшифровка записанной реплики.
   *
   * Текст берётся только у распознавания браузера — это речь самого
   * обучающегося. Если распознавания нет, поле остаётся пустым и заполняется
   * вручную: подставлять реплику из сценария вместо распознанной речи нельзя,
   * иначе транскрипт и оценка разговора станут вымыслом.
   */
  const transcribe = useCallback(
    async (_rec: Recording) => {
      const run = ++sttRun.current;
      setStt('pending');
      const { result, reason } = await speech.stop();
      if (run !== sttRun.current) return;
      if (!result) {
        setSttReason(reason);
        setSttLocal(false);
        setStt('manual');
        return;
      }
      setDraft(result.text);
      setSttReason(null);
      setSttLocal(result.local);
      setStt('ready');
    },
    [speech],
  );

  // Авто-стоп по лимиту длительности вызывает расшифровку через эту ссылку.
  useEffect(() => {
    transcribeRef.current = transcribe;
  }, [transcribe]);

  function discardClip() {
    sttRun.current += 1;
    speech.cancel();
    setClip(null);
    setStt('idle');
    setSttReason(null);
    setDraft('');
  }

  const recording = recorder.recording;
  const { start: startRecording, stop: stopRecording } = recorder;

  const startPtt = useCallback(async () => {
    if (recording || starting || disabled || phase !== 'idle' || state?.callEnded) return;
    setNotice(null);
    // Новая запись заменяет прежнюю вместе с её расшифровкой.
    sttRun.current += 1;
    setStt('idle');
    setSttReason(null);
    onEvent('ptt_start');
    /*
     * Доступ к микрофону выдаётся асинхронно. Пока он выдаётся, кнопка
     * показывает «Включаем микрофон…», а отпускание кнопки старт не отменяет:
     * иначе первое нажатие (когда браузер спрашивает разрешение) пропадало бы.
     */
    setStarting(true);
    const ok = await startRecording();
    // Распознавание слушает ту же реплику параллельно с записью.
    if (ok) await speech.start();
    setStarting(false);
    if (!micChecked.current) {
      micChecked.current = true;
      onEvent('mic_check', { ok });
    }
  }, [disabled, onEvent, phase, recording, speech, startRecording, starting, state?.callEnded]);

  const stopPtt = useCallback(async () => {
    const wasRecording = recording;
    const result = await stopRecording();
    if (!result) {
      if (wasRecording) {
        speech.cancel();
        onEvent('ptt_stop', { durationMs: 0 });
        setNotice({
          kind: 'noSpeech',
          text: 'Запись слишком короткая. Нажмите «Начать запись», скажите реплику и нажмите «Остановить запись».',
        });
      }
      return;
    }
    onEvent('ptt_stop', { durationMs: result.durationMs, mime: result.mime });
    // Запись не уходит сразу: сначала расшифровка, её проверяет обучающийся.
    setClip(result);
    void transcribe(result);
  }, [onEvent, recording, speech, stopRecording, transcribe]);

  /**
   * Одно действие на кнопке: нажатие начинает или заканчивает запись.
   * Удержание дольше HOLD_MS работает как рация — отпустили, запись кончилась.
   */
  const pressRecord = useCallback(() => {
    pressedAt.current = Date.now();
    if (recording) {
      void stopPtt();
      return;
    }
    if (starting) return;
    void startPtt();
  }, [recording, starting, startPtt, stopPtt]);

  const releaseRecord = useCallback(() => {
    const held = Date.now() - pressedAt.current;
    pressedAt.current = 0;
    // Короткое нажатие оставляет запись идти: остановит второе нажатие.
    if (held < HOLD_MS || !recording) return;
    void stopPtt();
  }, [recording, stopPtt]);

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
  const transcribing = stt === 'pending';
  const canSend = !composeDisabled && !busy && !recording && !transcribing && draft.trim().length > 0;
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
              {turn.speaker === 'caller' && turn.source === 'script' && turn.turnNo > 0 ? ' · по сценарию (ИИ недоступен)' : ''}
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
                ? speech.partial
                  ? `Идёт запись. Слышу: «${speech.partial}»`
                  : 'Идёт запись. Нажмите «Остановить запись», когда договорите.'
                : recorder.mic === 'requesting'
                  ? 'Запрашиваем доступ к микрофону…'
                  : voiceInput && micProblem
                    ? micProblem
                    : clip
                      ? stt === 'manual' && sttReason
                        ? `Аудиозапись сохранена. ${speechFailureText(sttReason)}`
                        : STT_STATUS[stt]
                      : 'Можно задать вопрос заявителю'}
      </div>

      {!ended && (
        <div className="call-compose">
          <label className="call-compose__field">
            <span className="arm-fld__label">
              {clip ? 'Расшифровка записанной реплики' : 'Реплика оператора'}
              {clip && stt === 'ready' && (
                <span className="dim"> · распознано браузером{sttLocal ? ' на устройстве' : ''}</span>
              )}
              {clip && stt === 'manual' && <span className="dim"> · ввод вручную</span>}
            </span>
            <input
              className="arm-line-input"
              value={draft}
              disabled={composeDisabled || busy || transcribing}
              placeholder={
                transcribing
                  ? 'Распознавание речи…'
                  : clip
                    ? 'Введите то, что вы сказали'
                    : 'Введите вопрос заявителю'
              }
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
              <button type="button" className="arm-mini" disabled={busy} onClick={discardClip}>
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
                title={micProblem ?? 'Нажмите, чтобы начать и закончить запись. Кнопку можно и удерживать.'}
                /*
                 * Захват указателя: отпускание засчитывается кнопке, даже если
                 * курсор ушёл в сторону. Обработчика pointerleave нет намеренно —
                 * именно он обрывал запись при малейшем смещении мыши.
                 */
                onPointerDown={(e) => {
                  e.preventDefault();
                  try {
                    e.currentTarget.setPointerCapture?.(e.pointerId);
                  } catch {
                    // захват — удобство, а не условие записи: без него работает обычный сценарий
                  }
                  pressRecord();
                }}
                onPointerUp={releaseRecord}
                onPointerCancel={releaseRecord}
                onLostPointerCapture={releaseRecord}
                onKeyDown={(e: ReactKeyboardEvent) => {
                  // Пробел и Enter — только на самой кнопке, ввод текста не затрагивается.
                  if ((e.key === ' ' || e.key === 'Enter') && !e.repeat) {
                    e.preventDefault();
                    pressRecord();
                  }
                }}
              >
                {recording ? '● Остановить запись' : starting ? 'Включаем микрофон…' : 'Начать запись'}
              </button>
            )}

            <button
              type="button"
              className="arm-mini"
              disabled={!canSend}
              onClick={() => void send(draft.trim(), undefined, clip)}
            >
              {busy ? 'Отправка…' : transcribing ? 'Распознавание…' : 'Отправить'}
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
            {!voiceInput
              ? 'Реплики вводятся текстом — так настроено занятие.'
              : speechAvailable
                ? 'Нажмите «Начать запись», скажите реплику, нажмите «Остановить запись» (кнопку можно и удерживать). Что распознал браузер, попадёт в поле расшифровки: проверьте текст и нажмите «Отправить». Если распознать не удалось, введите реплику сами — отправится именно ваш текст.'
                : 'Браузер не умеет распознавать речь: запишите реплику и введите её текст в поле расшифровки. Отправится именно он, запись прикладывается к ходу.'}
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
