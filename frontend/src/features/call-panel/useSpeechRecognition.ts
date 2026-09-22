/**
 * Распознавание речи средствами браузера (Web Speech API).
 *
 * Это единственный доступный на фронте способ получить текст ИМЕННО ТОГО, что
 * сказал обучающийся: в контракте отдельного STT-endpoint нет — речь распознаёт
 * ai-service внутри POST /dialogue/turns, а его в проекте пока нет.
 *
 * Никаких заготовленных реплик здесь не возвращается: если распознавание
 * недоступно (нет поддержки, нет разрешения, нет сети для облачного движка
 * браузера, ничего не расслышано), метод возвращает null, и интерфейс честно
 * предлагает ввести расшифровку вручную.
 *
 * Сначала запрашивается распознавание на устройстве (`processLocally`) — оно
 * работает офлайн и не отправляет звук наружу. Если такого режима нет, браузер
 * использует собственный движок; в изолированном контуре он просто недоступен.
 */

import { useCallback, useRef, useState } from 'react';

export type SpeechFailure = 'unsupported' | 'denied' | 'no-speech' | 'network' | 'error';

export interface SpeechResult {
  text: string;
  /** распознано на устройстве, звук наружу не отправлялся */
  local: boolean;
}

interface RecognitionLike extends EventTarget {
  lang: string;
  continuous: boolean;
  interimResults: boolean;
  maxAlternatives: number;
  processLocally?: boolean;
  start(): void;
  stop(): void;
  abort(): void;
}

interface RecognitionCtor {
  new (): RecognitionLike;
  available?(options: { langs: string[]; processLocally: boolean }): Promise<string>;
}

interface RecognitionAlternative { transcript: string }
interface RecognitionResultItem { isFinal: boolean; 0: RecognitionAlternative; length: number }
interface RecognitionEvent extends Event { resultIndex: number; results: ArrayLike<RecognitionResultItem> }
interface RecognitionErrorEvent extends Event { error: string }

const LANG = 'ru-RU';
/** Сколько ждём финальный результат после остановки записи. */
const FINALIZE_MS = 2500;

function ctor(): RecognitionCtor | null {
  if (typeof window === 'undefined') return null;
  const w = window as unknown as { SpeechRecognition?: RecognitionCtor; webkitSpeechRecognition?: RecognitionCtor };
  return w.SpeechRecognition ?? w.webkitSpeechRecognition ?? null;
}

export function speechSupported(): boolean {
  return ctor() != null;
}

export function speechFailureText(reason: SpeechFailure): string {
  switch (reason) {
    case 'no-speech':
      return 'Речь в записи не распознана. Введите текст реплики или запишите её заново.';
    case 'denied':
      return 'Браузер не разрешил распознавание речи. Введите текст реплики.';
    case 'network':
      return 'Распознаванию речи в браузере недоступна сеть. Введите текст реплики.';
    default:
      return 'Автоматическая расшифровка недоступна. Введите текст реплики.';
  }
}

export function useSpeechRecognition() {
  /** Промежуточный текст, пока обучающийся говорит. */
  const [partial, setPartial] = useState('');
  const recognition = useRef<RecognitionLike | null>(null);
  const finalText = useRef('');
  const interimText = useRef('');
  const failure = useRef<SpeechFailure | null>(null);
  const ended = useRef<(() => void) | null>(null);

  const start = useCallback(async () => {
    const Ctor = ctor();
    if (!Ctor) {
      failure.current = 'unsupported';
      return false;
    }
    finalText.current = '';
    interimText.current = '';
    failure.current = null;
    setPartial('');

    const rec = new Ctor();
    rec.lang = LANG;
    rec.continuous = true;
    rec.interimResults = true;
    rec.maxAlternatives = 1;

    /*
     * Распознавание на устройстве: звук не уходит во внешний сервис. Если
     * модели нет, остаётся движок браузера — в изолированном контуре он не
     * заработает, и расшифровку введут вручную.
     */
    try {
      const availability = await Ctor.available?.({ langs: [LANG], processLocally: true });
      if (availability === 'available') rec.processLocally = true;
    } catch {
      // проверка доступности не обязательна
    }

    rec.addEventListener('result', (event) => {
      const e = event as RecognitionEvent;
      let interim = '';
      for (let i = e.resultIndex; i < e.results.length; i++) {
        const item = e.results[i];
        const text = item[0]?.transcript ?? '';
        if (item.isFinal) finalText.current += text;
        else interim += text;
      }
      interimText.current = interim;
      setPartial((finalText.current + interim).trim());
    });
    rec.addEventListener('error', (event) => {
      const code = (event as RecognitionErrorEvent).error;
      failure.current =
        code === 'not-allowed' || code === 'service-not-allowed'
          ? 'denied'
          : code === 'no-speech'
            ? 'no-speech'
            : code === 'network'
              ? 'network'
              : 'error';
    });
    rec.addEventListener('end', () => {
      const done = ended.current;
      ended.current = null;
      done?.();
    });

    try {
      rec.start();
    } catch {
      failure.current = 'error';
      return false;
    }
    recognition.current = rec;
    return true;
  }, []);

  /** Возвращает распознанный текст или причину, по которой его нет. */
  const stop = useCallback(async (): Promise<{ result: SpeechResult | null; reason: SpeechFailure | null }> => {
    const rec = recognition.current;
    recognition.current = null;
    if (!rec) return { result: null, reason: failure.current ?? 'unsupported' };

    await new Promise<void>((resolve) => {
      const timer = setTimeout(resolve, FINALIZE_MS);
      ended.current = () => {
        clearTimeout(timer);
        resolve();
      };
      try {
        rec.stop();
      } catch {
        clearTimeout(timer);
        resolve();
      }
    });

    const text = (finalText.current || interimText.current).trim();
    setPartial('');
    if (!text) return { result: null, reason: failure.current ?? 'no-speech' };
    return { result: { text, local: rec.processLocally === true }, reason: null };
  }, []);

  /** Прерывание без результата: запись отменили. */
  const cancel = useCallback(() => {
    const rec = recognition.current;
    recognition.current = null;
    ended.current = null;
    setPartial('');
    try {
      rec?.abort();
    } catch {
      // распознавание уже завершено
    }
  }, []);

  return { partial, start, stop, cancel };
}
