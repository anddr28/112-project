import { useState } from 'react';
import { Badge, Card, ErrorState, Loading } from '../../components/ui';
import { ROLE_LABEL, useAuth } from '../../app/auth';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { fullName } from '../../shared/utils/user';
import type { User } from '../../shared/types';

/**
 * Управление учётными записями.
 *
 * Блокировка — техническая операция администратора: причина не требуется,
 * журнала банов нет. Проверка выполняется в сервисном слое, а не в разметке:
 * заблокированный пользователь не может войти, и его действующая сессия
 * перестаёт давать доступ при следующей проверке авторизации.
 *
 * TODO(backend) J-01..J-03: создание учётных записей и смена роли требуют
 * контракта, поэтому эти действия недоступны.
 */
export function AdminUsersPage() {
  const users = useAsync(() => api.users.list(), []);
  const current = useAuth((s) => s.user);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  if (users.loading) return <Loading />;
  if (users.error) return <ErrorState text={users.error} onRetry={users.reload} />;

  const list = users.data ?? [];
  const blocked = list.filter((u) => u.status === 'blocked').length;

  async function toggle(user: User) {
    setBusyId(user.id);
    setError(null);
    try {
      await api.users.setBlocked(user.id, user.status !== 'blocked');
      users.reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Не удалось изменить состояние учётной записи');
    } finally {
      setBusyId(null);
    }
  }

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Пользователи</h1>
          <div className="page-head__sub">Учётные записи, роли и профили ДДС.</div>
        </div>
        <div className="page-head__actions">
          <button type="button" className="btn btn--primary" disabled title="Действие выполняется на сервере: контракт ещё не согласован">
            Создать пользователя
          </button>
        </div>
      </div>

      {error && <p className="field__error" role="alert" style={{ marginBottom: 12 }}>{error}</p>}

      <Card title={`Всего: ${list.length}${blocked ? ` · заблокировано: ${blocked}` : ''}`}>
        <table className="table">
          <thead>
            <tr><th>ФИО</th><th>Логин</th><th>Роль</th><th>Профиль ДДС</th><th>Состояние</th><th /></tr>
          </thead>
          <tbody>
            {list.map((u) => {
              const isSelf = u.id === current?.id;
              const isBlocked = u.status === 'blocked';
              return (
                <tr key={u.id}>
                  <td>{fullName(u)}</td>
                  <td className="mono small">{u.login}</td>
                  <td>{ROLE_LABEL[u.role]}</td>
                  <td className="muted small">{u.serviceName ?? '—'}</td>
                  <td>
                    <Badge tone={isBlocked ? 'danger' : 'ok'}>
                      {isBlocked ? 'заблокирован' : 'активен'}
                    </Badge>
                  </td>
                  <td>
                    <button
                      type="button"
                      className={`btn btn--sm${isBlocked ? ' btn--primary' : ''}`}
                      disabled={isSelf || busyId === u.id}
                      title={isSelf ? 'Нельзя заблокировать текущую учётную запись' : undefined}
                      onClick={() => void toggle(u)}
                    >
                      {busyId === u.id ? 'Применение…' : isBlocked ? 'Разблокировать' : 'Заблокировать'}
                    </button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>

        <p className="field__hint" style={{ marginTop: 12 }}>
          Профиль ДДС определяет, какие профильные события попадают обучающемуся.
          Заблокированный пользователь не может войти в систему, а его открытая
          сессия перестаёт давать доступ при следующей проверке авторизации.
        </p>
      </Card>
    </>
  );
}
