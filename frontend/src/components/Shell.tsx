import { useEffect, useState } from 'react';
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom';
import { ROLE_LABEL, useAuth } from '../app/auth';
import { useEscape } from './useEscape';
import { shortName } from '../shared/utils/user';
import type { Role } from '../shared/types';

/**
 * Оболочка системы.
 *
 * Шапка построена по логике реального АРМ-112: слева обозначение системы,
 * затем разделы, справа — кто работает, на каком рабочем месте и текущее
 * время. Никаких «таблеток» навигации: активный раздел отмечается
 * подчёркиванием, как в ведомственных системах.
 */

interface NavItem {
  to: string;
  label: string;
  end?: boolean;
}

const NAV: Record<Role, NavItem[]> = {
  teacher: [
    { to: '/teacher', label: 'Обзор', end: true },
    { to: '/teacher/scenarios', label: 'Сценарии' },
    { to: '/teacher/lessons', label: 'Занятия' },
    { to: '/teacher/analytics', label: 'Аналитика' },
    { to: '/materials', label: 'Справочная база' },
  ],
  student: [
    { to: '/student', label: 'Мои занятия', end: true },
    { to: '/student/progress', label: 'Мой прогресс' },
    { to: '/materials', label: 'Справочная база' },
  ],
  // Разделов у администратора много — подписи короткие, чтобы полоса не прокручивалась.
  admin: [
    { to: '/admin', label: 'Состояние', end: true },
    { to: '/admin/users', label: 'Пользователи' },
    { to: '/admin/analytics', label: 'Аналитика' },
    { to: '/admin/audit', label: 'Аудит' },
    { to: '/admin/backups', label: 'Копии' },
    { to: '/admin/logs', label: 'Журнал' },
    { to: '/admin/settings', label: 'Настройки' },
    { to: '/materials', label: 'Справка' },
  ],
};

export function Shell() {
  const user = useAuth((s) => s.user);
  const logout = useAuth((s) => s.logout);
  const navigate = useNavigate();
  const location = useLocation();
  /*
   * Меню на узком экране (телефон в локальной сети — ТЗ): разделы и учётная
   * запись. Открыто «для страницы»: переход на другую закрывает его сам —
   * иначе меню заслоняло бы открытый раздел.
   */
  const [menuFor, setMenuFor] = useState<string | null>(null);
  const menuOpen = menuFor === location.pathname;
  const setMenuOpen = (open: boolean) => setMenuFor(open ? location.pathname : null);
  useEscape(() => setMenuFor(null));

  if (!user) return null;

  return (
    <div className={`shell${menuOpen ? ' shell--menu' : ''}`}>
      <header className="shell__top">
        <button
          type="button"
          className="shell__burger"
          aria-label={menuOpen ? 'Закрыть меню' : 'Открыть меню'}
          aria-expanded={menuOpen}
          aria-controls="shell-nav"
          onClick={() => setMenuOpen(!menuOpen)}
        >
          <span aria-hidden="true" />
        </button>
        <div className="shell__brand">
          <span className="shell__logo">112</span>
          <span className="shell__brand-text">
            <span className="shell__brand-title">Тренажёр АРМ-112</span>
            <span className="shell__brand-sub">Учебный контур</span>
          </span>
        </div>

        <nav className="shell__nav" id="shell-nav" aria-label="Разделы">
          {NAV[user.role].map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) => (isActive ? 'is-active' : undefined)}
            >
              {item.label}
            </NavLink>
          ))}
        </nav>

        <div className="shell__spacer" />

        <div className="shell__meta">
          <span className="shell__meta-item">
            <span className="shell__meta-label">{ROLE_LABEL[user.role]}</span>
            <span className="shell__meta-value">{shortName(user)}</span>
          </span>

          {user.workstation && (
            <span className="shell__meta-item">
              <span className="shell__meta-label">Рабочее место</span>
              <span className="shell__meta-value">{user.workstation}</span>
            </span>
          )}

          {user.serviceName && (
            <span className="shell__meta-item">
              <span className="shell__meta-label">Профиль</span>
              <span className="shell__meta-value">{user.serviceName}</span>
            </span>
          )}

          <Clock />

          <button
            type="button"
            className="btn btn--sm"
            onClick={() => {
              void logout().then(() => navigate('/login', { replace: true }));
            }}
          >
            Выход
          </button>
        </div>
      </header>

      <main className="shell__main">
        <Outlet />
      </main>
    </div>
  );
}

/**
 * Часы смены — как в шапке реального рабочего места.
 *
 * Отдельный компонент, а не хук в Shell: иначе секундное обновление
 * перерисовывало бы весь раздел вместе с таблицами и карточками.
 */
function Clock() {
  const [value, setValue] = useState(() => new Date().toLocaleTimeString('ru-RU', { hour12: false }));

  useEffect(() => {
    const id = setInterval(
      () => setValue(new Date().toLocaleTimeString('ru-RU', { hour12: false })),
      1000,
    );
    return () => clearInterval(id);
  }, []);

  return <span className="shell__clock">{value}</span>;
}
