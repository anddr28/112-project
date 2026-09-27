import { useState } from 'react';
import { Badge, Card, ErrorState, Field, Loading, Modal } from '../../components/ui';
import { ROLE_LABEL, useAuth } from '../../app/auth';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { fullName } from '../../shared/utils/user';
import type { CreateUserInput } from '../../shared/api';
import type { Role, User } from '../../shared/types';

/**
 * Управление учётными записями.
 *
 * Блокировка — техническая операция администратора: причина не требуется,
 * журнала банов нет. Проверка выполняется в сервисном слое, а не в разметке:
 * заблокированный пользователь не может войти, и его действующая сессия
 * перестаёт давать доступ при следующей проверке авторизации.
 *
 * Создание учётной записи — POST /users (у mock-реализации нет). Смена роли и
 * ФИО (PATCH /users/{id}) в контракте есть, экрана под неё пока нет.
 */
export function AdminUsersPage() {
  const users = useAsync(() => api.users.list(), []);
  const current = useAuth((s) => s.user);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const createUser = api.users.create;

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
          <button
            type="button"
            className="btn btn--primary"
            disabled={!createUser}
            title={createUser ? undefined : 'Учётные записи создаёт сервер; в демонстрационном режиме без сервера недоступно'}
            onClick={() => setCreating(true)}
          >
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

      {creating && createUser && (
        <CreateUserModal
          create={createUser}
          onClose={() => setCreating(false)}
          onCreated={() => {
            setCreating(false);
            users.reload();
          }}
        />
      )}
    </>
  );
}

const ROLES: Role[] = ['student', 'teacher', 'admin'];
/** Контракт: логин от 3 символов, пароль от 8. */
const MIN_LOGIN = 3;
const MIN_PASSWORD = 8;

function CreateUserModal({
  create,
  onClose,
  onCreated,
}: {
  create: (input: CreateUserInput) => Promise<User>;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [form, setForm] = useState<CreateUserInput>({ login: '', password: '', role: 'student', lastName: '', firstName: '', middleName: '' });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const patch = (next: Partial<CreateUserInput>) => setForm((prev) => ({ ...prev, ...next }));

  const loginError = form.login.trim().length > 0 && form.login.trim().length < MIN_LOGIN ? `Не короче ${MIN_LOGIN} символов` : undefined;
  const passwordError = form.password.length > 0 && form.password.length < MIN_PASSWORD ? `Не короче ${MIN_PASSWORD} символов` : undefined;
  const ready = form.login.trim().length >= MIN_LOGIN && form.password.length >= MIN_PASSWORD
    && form.lastName.trim() !== '' && form.firstName.trim() !== '';

  async function submit() {
    setBusy(true);
    setError(null);
    try {
      await create({
        ...form,
        login: form.login.trim(),
        lastName: form.lastName.trim(),
        firstName: form.firstName.trim(),
        middleName: form.middleName?.trim() || undefined,
      });
      onCreated();
    } catch (e) {
      // 409 — логин занят: сервер присылает понятный текст.
      setError(e instanceof Error ? e.message : 'Не удалось создать учётную запись');
      setBusy(false);
    }
  }

  return (
    <Modal
      title="Новая учётная запись"
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={busy}>Отмена</button>
          <button type="button" className="btn btn--primary" onClick={() => void submit()} disabled={!ready || busy}>
            {busy ? 'Создание…' : 'Создать'}
          </button>
        </>
      }
    >
      <div className="stack" style={{ gap: 12 }}>
        {error && <p className="field__error" role="alert">{error}</p>}
        <div className="grid grid--3">
          <Field label="Фамилия">
            <input className="input" value={form.lastName} onChange={(e) => patch({ lastName: e.target.value })} />
          </Field>
          <Field label="Имя">
            <input className="input" value={form.firstName} onChange={(e) => patch({ firstName: e.target.value })} />
          </Field>
          <Field label="Отчество">
            <input className="input" value={form.middleName ?? ''} onChange={(e) => patch({ middleName: e.target.value })} />
          </Field>
        </div>
        <div className="grid grid--3">
          <Field label="Логин" error={loginError}>
            <input className="input" autoComplete="off" value={form.login} onChange={(e) => patch({ login: e.target.value })} />
          </Field>
          <Field label="Пароль" error={passwordError} hint="Пароль хранится на сервере только в виде хэша">
            <input className="input" type="password" autoComplete="new-password" value={form.password} onChange={(e) => patch({ password: e.target.value })} />
          </Field>
          <Field label="Роль">
            <select className="select" value={form.role} onChange={(e) => patch({ role: e.target.value as Role })}>
              {ROLES.map((r) => <option key={r} value={r}>{ROLE_LABEL[r]}</option>)}
            </select>
          </Field>
        </div>
      </div>
    </Modal>
  );
}
