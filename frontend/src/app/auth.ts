import { create } from 'zustand';
import { api } from '../shared/api';
import { onSessionLost } from '../shared/api/session';
import type { Role, User } from '../shared/types';

interface AuthState {
  user: User | null;
  loading: boolean;
  error: string | null;
  restore: () => Promise<void>;
  login: (login: string, password: string) => Promise<User>;
  logout: () => Promise<void>;
}

export const useAuth = create<AuthState>((set) => ({
  user: null,
  loading: true,
  error: null,

  async restore() {
    set({ loading: true, error: null });
    try {
      const user = await api.auth.me();
      set({ user, loading: false });
    } catch (e) {
      // Сессию восстановить не удалось — показываем вход, а не бесконечную загрузку.
      set({
        user: null,
        loading: false,
        error: e instanceof Error ? e.message : 'Не удалось восстановить сессию',
      });
    }
  },

  async login(login, password) {
    set({ loading: true, error: null });
    try {
      const user = await api.auth.login({ login, password });
      set({ user, loading: false });
      return user;
    } catch (e) {
      const message = e instanceof Error ? e.message : 'Ошибка входа';
      set({ loading: false, error: message });
      throw e;
    }
  },

  async logout() {
    /*
     * Локальный сеанс закрываем в любом случае: если запрос не прошёл,
     * пользователь всё равно должен выйти, а не остаться в системе с
     * неработающей кнопкой.
     */
    try {
      await api.auth.logout();
    } finally {
      set({ user: null, error: null });
    }
  },
}));

/*
 * Сессия потеряна посреди работы (истекла, отозвана, учётку заблокировали):
 * закрываем локальный сеанс — защищённые маршруты сами уведут на вход, где
 * пользователь увидит причину.
 */
onSessionLost((reason) => {
  if (!useAuth.getState().user) return;
  useAuth.setState({
    user: null,
    error: reason === 'user_blocked' ? 'Учётная запись заблокирована.' : 'Сеанс завершён. Войдите снова.',
  });
});

export const ROLE_LABEL: Record<Role, string> = {
  admin: 'Администратор',
  teacher: 'Преподаватель',
  student: 'Обучающийся',
};

/** Стартовый маршрут роли — используется и при логине, и при заходе на «/». */
export function homeFor(role: Role): string {
  if (role === 'teacher') return '/teacher';
  if (role === 'student') return '/student';
  return '/admin';
}
