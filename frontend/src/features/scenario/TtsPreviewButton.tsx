import { useEffect, useRef, useState } from 'react';
import { api, isApiError } from '../../shared/api';
import { speakRussian } from '../../shared/utils/speech';

type State = { phase: 'idle' | 'loading' | 'playing' } | { phase: 'note'; text: string; tone: 'warn' | 'danger' };

/**
 * «Прослушать» реплику голосом заявителя (POST /scenarios/{id}/tts-preview).
 *
 * Синтез синхронный и зависит от ai-service: 503 (`ai_unavailable`,
 * `caller_busy`) — не ошибка экрана, а повод прочитать реплику голосом
 * браузера и честно подписать, что это не голос заявителя из сценария.
 */
export function TtsPreviewButton({ scenarioId, text }: { scenarioId: string; text: string }) {
  const [state, setState] = useState<State>({ phase: 'idle' });
  const audio = useRef<HTMLAudioElement | null>(null);

  // Ушли со страницы — звук не должен продолжаться.
  useEffect(() => () => audio.current?.pause(), []);

  async function play() {
    audio.current?.pause();
    setState({ phase: 'loading' });
    try {
      const ref = await api.scenarios.ttsPreview(scenarioId, text.slice(0, 500));
      const el = new Audio(ref.audioUrl);
      audio.current = el;
      el.onended = () => setState({ phase: 'idle' });
      await el.play();
      setState({ phase: 'playing' });
    } catch (e) {
      const unavailable = isApiError(e) && (e.code === 'ai_unavailable' || e.code === 'caller_busy');
      if (speakRussian(text)) {
        setState({ phase: 'note', tone: 'warn', text: unavailable ? 'Синтез речи недоступен — прочитано голосом браузера' : 'Аудио не воспроизвелось — прочитано голосом браузера' });
        return;
      }
      setState({
        phase: 'note',
        tone: 'danger',
        text: unavailable ? 'Синтез речи сейчас недоступен, повторите позже' : e instanceof Error ? e.message : 'Не удалось воспроизвести реплику',
      });
    }
  }

  return (
    <span className="row row--tight">
      <button
        type="button"
        className="btn btn--sm"
        disabled={!text.trim() || state.phase === 'loading'}
        onClick={() => {
          if (state.phase === 'playing') {
            audio.current?.pause();
            setState({ phase: 'idle' });
            return;
          }
          void play();
        }}
      >
        {state.phase === 'loading' ? 'Синтез…' : state.phase === 'playing' ? '■ Стоп' : '▶ Прослушать'}
      </button>
      {state.phase === 'note' && (
        <span className="small" role="status" style={{ color: state.tone === 'danger' ? 'var(--u-danger)' : 'var(--u-warn)' }}>
          {state.text}
        </span>
      )}
    </span>
  );
}
