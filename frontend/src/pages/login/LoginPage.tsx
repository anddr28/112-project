import { useEffect, useId, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { homeFor, useAuth } from '../../app/auth';
import { CitySkyline } from './CitySkyline';
import { api } from '../../shared/api';
import type { DemoAccount } from '../../shared/api';

/**
 * Вход в систему.
 *
 * Композиция и элементы — по mockups/pov112-login.png: иллюстрация во всё поле,
 * форма у правого края без панели, подчёркнутые поля, серая кнопка «ВОЙТИ»,
 * блок технической поддержки.
 *
 * Контакты поддержки намеренно нейтральные: тренажёр не должен отправлять
 * обучающихся в службу поддержки боевой системы. Метка «учебный контур»
 * по той же причине — чтобы экран не выдавал себя за производственный ПОВ-112.
 */
export function LoginPage() {
  const [login, setLogin] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const doLogin = useAuth((s) => s.login);
  /*
   * Сессию могли прервать снаружи — например, администратор заблокировал
   * учётную запись. Причина хранится в сторе авторизации, и показать её
   * нужно здесь, иначе пользователя просто выбросило бы на экран входа.
   */
  const restoreError = useAuth((s) => s.error);
  const navigate = useNavigate();

  const loginId = useId();
  const passwordId = useId();
  const [accounts, setAccounts] = useState<DemoAccount[]>([]);

  useEffect(() => {
    let cancelled = false;
    // Демо-учётки — подсказка, а не условие входа: сервер недоступен — просто не показываем.
    api.auth.demoAccounts().then(
      (list) => {
        if (!cancelled) setAccounts(list);
      },
      () => {},
    );
    return () => { cancelled = true; };
  }, []);

  async function submit(nextLogin: string, nextPassword: string) {
    setBusy(true);
    setError(null);
    try {
      const user = await doLogin(nextLogin, nextPassword);
      navigate(homeFor(user.role), { replace: true });
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Не удалось выполнить вход');
      setBusy(false);
    }
  }

  const canSubmit = login.trim().length > 0 && password.length > 0 && !busy;

  return (
    <div className="login">
      <CitySkyline />
      <div className="login__scrim" />

      <div className="login__layout">
        <div className="login__form">
          <div className="login__mark">Учебный контур</div>

          <div className="login__lockup">
            <span className="login__112">112</span>
            <span className="login__title">Вход в систему</span>
          </div>

          {(error ?? restoreError) && (
            <div className="login__error" role="alert">
              <span aria-hidden="true">!</span>
              <span>{error ?? restoreError}</span>
            </div>
          )}

          <form
            onSubmit={(e) => {
              e.preventDefault();
              if (canSubmit) void submit(login, password);
            }}
          >
            <div className="login__fields">
              <div className={`login__field${error ? ' login__field--invalid' : ''}`}>
                <label className="login__label" htmlFor={loginId}>логин:</label>
                <input
                  id={loginId}
                  className="login__input"
                  value={login}
                  disabled={busy}
                  autoComplete="username"
                  autoFocus
                  onChange={(e) => setLogin(e.target.value)}
                />
              </div>

              <div className={`login__field${error ? ' login__field--invalid' : ''}`}>
                <label className="login__label" htmlFor={passwordId}>пароль:</label>
                <input
                  id={passwordId}
                  className="login__input"
                  type="password"
                  value={password}
                  disabled={busy}
                  autoComplete="current-password"
                  onChange={(e) => setPassword(e.target.value)}
                />
              </div>
            </div>

            <button type="submit" className="login__submit" disabled={!canSubmit}>
              {busy ? (
                <span className="login__submit-row">
                  <span className="spinner" aria-hidden="true" />
                  Проверка
                </span>
              ) : (
                'Войти'
              )}
            </button>
          </form>

          <div className="login__support">
            <div className="login__support-title">Техническая поддержка учебного контура</div>
            <div className="login__support-note">
              По вопросам доступа обратитесь к преподавателю или администратору учебного класса.
            </div>
          </div>

          {accounts.length > 0 && (
          <div className="login__accounts">
            <div className="login__accounts-label">Учебные учётные записи</div>
            <div className="login__accounts-row">
              {accounts.map((acc) => (
                <button
                  key={acc.login}
                  type="button"
                  className="login__account"
                  disabled={busy}
                  onClick={() => {
                    setLogin(acc.login);
                    setPassword(acc.password);
                    void submit(acc.login, acc.password);
                  }}
                >
                  {acc.label}
                </button>
              ))}
            </div>
          </div>
          )}
        </div>
      </div>
    </div>
  );
}
