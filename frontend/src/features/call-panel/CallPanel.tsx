import { useEffect, useRef, useState } from 'react';
import { cls } from '../../shared/utils/cls';
import type { StudentCallScript } from '../../shared/types';

/**
 * Панель разговора с заявителем.
 *
 * Реплики открываются последовательно: текущая подсвечена, предыдущие
 * остаются историей. Кнопка «переспросить» повторяет реплику и увеличивает
 * `replay_count` попытки (учитывается при оценке).
 *
 * TODO(backend) GAP-10 / GAP-14 / GAP-15: контракт `CallScript.turns` даёт
 * только `{speaker, text, ttsHash}` — ни URL аудио, ни длительности.
 * Пока `audioUrl` отсутствует, панель работает как текстовая расшифровка;
 * при появлении раздачи TTS сюда добавляется плеер без изменения разметки.
 */
export function CallPanel({
  script,
  onReplay,
}: {
  script: StudentCallScript;
  onReplay: () => void;
}) {
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
