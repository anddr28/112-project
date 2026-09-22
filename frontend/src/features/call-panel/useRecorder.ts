import { useCallback, useEffect, useRef, useState } from 'react';

/**
 * Запись реплики оператора с микрофона: MediaRecorder + уровень сигнала.
 *
 * Микрофон запрашивается при первом нажатии «Говорить», а не при открытии
 * АРМ: браузер показывает запрос разрешения только по жесту пользователя,
 * и обучающийся, работающий текстом, запроса не увидит вовсе.
 *
 * Ограничение браузера: getUserMedia доступен только в защищённом контексте
 * (HTTPS или localhost). Поэтому voice-mode требует HTTPS для SPA.
 */

export type MicState = 'unknown' | 'requesting' | 'ready' | 'denied' | 'missing' | 'insecure' | 'unsupported' | 'error';

export interface Recording {
  blob: Blob;
  url: string;
  durationMs: number;
  mime: string;
  recordedAt: string;
}

/** Короче этого — случайное касание кнопки, а не реплика. */
export const MIN_RECORDING_MS = 400;
/** Контракт ai-service ограничивает длину реплики; дольше — обрезаем запись. */
export const MAX_RECORDING_MS = 30_000;

const MIME_CANDIDATES = ['audio/webm;codecs=opus', 'audio/webm', 'audio/ogg;codecs=opus', 'audio/mp4'];

function pickMime(): string {
  if (typeof MediaRecorder === 'undefined') return '';
  return MIME_CANDIDATES.find((m) => MediaRecorder.isTypeSupported(m)) ?? '';
}

export function micStateText(state: MicState): string | null {
  switch (state) {
    case 'denied':
      return 'Доступ к микрофону запрещён в браузере. Разрешите его в настройках сайта или продолжайте текстом.';
    case 'missing':
      return 'Микрофон не найден. Подключите гарнитуру или продолжайте текстом.';
    case 'insecure':
      return 'Запись голоса доступна только по HTTPS. Продолжайте текстом.';
    case 'unsupported':
      return 'Браузер не поддерживает запись звука. Продолжайте текстом.';
    case 'error':
      return 'Не удалось включить микрофон. Продолжайте текстом.';
    default:
      return null;
  }
}

export function useRecorder() {
  const [mic, setMic] = useState<MicState>('unknown');
  const [recording, setRecording] = useState(false);
  const [elapsedMs, setElapsedMs] = useState(0);
  const [level, setLevel] = useState(0);

  const stream = useRef<MediaStream | null>(null);
  const recorder = useRef<MediaRecorder | null>(null);
  const chunks = useRef<Blob[]>([]);
  const startedAt = useRef(0);
  const audioCtx = useRef<AudioContext | null>(null);
  const analyser = useRef<AnalyserNode | null>(null);
  const raf = useRef<number | null>(null);
  const autoStop = useRef<ReturnType<typeof setTimeout> | null>(null);
  /** Отпускание кнопки раньше, чем браузер выдал поток: запись начинать не нужно. */
  const cancelled = useRef(false);
  const resolveStop = useRef<((r: Recording | null) => void) | null>(null);
  /** Запись, остановленная по лимиту длины до того, как кнопку отпустили. */
  const autoResult = useRef<Recording | null>(null);

  const stopMeter = useCallback(() => {
    if (raf.current != null) cancelAnimationFrame(raf.current);
    raf.current = null;
    setLevel(0);
  }, []);

  const release = useCallback(() => {
    stopMeter();
    stream.current?.getTracks().forEach((t) => t.stop());
    stream.current = null;
    void audioCtx.current?.close().catch(() => {});
    audioCtx.current = null;
    analyser.current = null;
  }, [stopMeter]);

  useEffect(() => () => {
    if (autoStop.current) clearTimeout(autoStop.current);
    if (recorder.current?.state === 'recording') recorder.current.stop();
    release();
  }, [release]);

  /** Поток держим открытым между репликами: повторный запрос дал бы задержку. */
  const ensureStream = useCallback(async (): Promise<MediaStream | null> => {
    if (stream.current) return stream.current;
    if (!window.isSecureContext) {
      setMic('insecure');
      return null;
    }
    if (!navigator.mediaDevices?.getUserMedia || typeof MediaRecorder === 'undefined') {
      setMic('unsupported');
      return null;
    }
    setMic('requesting');
    try {
      const s = await navigator.mediaDevices.getUserMedia({
        audio: { echoCancellation: true, noiseSuppression: true, channelCount: 1 },
      });
      stream.current = s;
      try {
        const ctx = new AudioContext();
        const node = ctx.createAnalyser();
        node.fftSize = 512;
        ctx.createMediaStreamSource(s).connect(node);
        // Созданный после await контекст Chrome может оставить «спящим».
        void ctx.resume().catch(() => {});
        audioCtx.current = ctx;
        analyser.current = node;
      } catch {
        // индикатор уровня — вспомогательный: запись работает и без него
      }
      setMic('ready');
      return s;
    } catch (e) {
      const name = e instanceof DOMException ? e.name : '';
      setMic(name === 'NotAllowedError' || name === 'SecurityError'
        ? 'denied'
        : name === 'NotFoundError' || name === 'OverconstrainedError'
          ? 'missing'
          : 'error');
      return null;
    }
  }, []);

  const runMeter = useCallback(() => {
    const node = analyser.current;
    if (!node) return;
    const data = new Uint8Array(node.fftSize);
    const tick = () => {
      node.getByteTimeDomainData(data);
      let peak = 0;
      for (const v of data) peak = Math.max(peak, Math.abs(v - 128));
      setLevel(Math.min(1, peak / 64));
      setElapsedMs(Date.now() - startedAt.current);
      raf.current = requestAnimationFrame(tick);
    };
    raf.current = requestAnimationFrame(tick);
  }, []);

  const start = useCallback(async (): Promise<boolean> => {
    if (recorder.current?.state === 'recording') return true;
    cancelled.current = false;
    const s = await ensureStream();
    if (!s || cancelled.current) return false;

    const mime = pickMime();
    let rec: MediaRecorder;
    try {
      rec = mime ? new MediaRecorder(s, { mimeType: mime }) : new MediaRecorder(s);
    } catch {
      setMic('unsupported');
      return false;
    }
    chunks.current = [];
    rec.ondataavailable = (ev) => {
      if (ev.data.size > 0) chunks.current.push(ev.data);
    };
    rec.onstop = () => {
      const durationMs = Date.now() - startedAt.current;
      const type = rec.mimeType || mime || 'audio/webm';
      const blob = new Blob(chunks.current, { type });
      chunks.current = [];
      const result: Recording | null =
        durationMs < MIN_RECORDING_MS || blob.size === 0
          ? null
          : { blob, url: URL.createObjectURL(blob), durationMs, mime: type, recordedAt: new Date(startedAt.current).toISOString() };
      const done = resolveStop.current;
      resolveStop.current = null;
      if (done) done(result);
      else autoResult.current = result;
    };
    recorder.current = rec;
    startedAt.current = Date.now();
    setElapsedMs(0);
    rec.start(250);
    setRecording(true);
    runMeter();
    autoResult.current = null;
    autoStop.current = setTimeout(() => {
      if (rec.state !== 'recording') return;
      stopMeter();
      rec.stop();
    }, MAX_RECORDING_MS);
    return true;
  }, [ensureStream, runMeter, stopMeter]);

  /** Останавливает запись. `null` — запись слишком короткая или пустая. */
  const stop = useCallback((): Promise<Recording | null> => {
    cancelled.current = true;
    if (autoStop.current) clearTimeout(autoStop.current);
    autoStop.current = null;
    stopMeter();
    setRecording(false);
    const rec = recorder.current;
    if (!rec || rec.state !== 'recording') {
      const auto = autoResult.current;
      autoResult.current = null;
      return Promise.resolve(auto);
    }
    return new Promise((resolve) => {
      resolveStop.current = resolve;
      rec.stop();
    });
  }, [stopMeter]);

  return { mic, recording, elapsedMs, level, start, stop };
}
