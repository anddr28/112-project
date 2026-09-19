import type { ReactNode } from 'react';
import { Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { homeFor, useAuth } from './auth';
import { Shell } from '../components/Shell';
import { Loading } from '../components/ui';
import { LoginPage } from '../pages/login/LoginPage';
import { TeacherDashboard } from '../pages/teacher/TeacherDashboard';
import { ScenarioListPage } from '../pages/teacher/ScenarioListPage';
import { ScenarioDetailPage } from '../pages/teacher/ScenarioDetailPage';
import { LessonListPage } from '../pages/teacher/LessonListPage';
import { LessonDetailPage } from '../pages/teacher/LessonDetailPage';
import { AttemptReportPage } from '../pages/teacher/AttemptReportPage';
import { StudentDashboard } from '../pages/student/StudentDashboard';
import { StudentLessonPage } from '../pages/student/StudentLessonPage';
import { IncomingCallPage } from '../pages/student/IncomingCallPage';
import { Arm112Page } from '../pages/student/Arm112Page';
import { ResultPage } from '../pages/student/ResultPage';
import { AdminDashboard } from '../pages/admin/AdminDashboard';
import { AdminUsersPage } from '../pages/admin/AdminUsersPage';
import { ForbiddenPage, NotFoundPage } from '../pages/errors/ErrorPages';
import type { Role } from '../shared/types';

/**
 * RBAC на уровне маршрутов.
 *
 * Скрытие маршрута — не безопасность: backend обязан проверять права заново.
 * Поэтому запрещённый маршрут отдаёт честный 403, а не 404 и не пустой экран.
 */
function Protected({ roles, children }: { roles: Role[]; children: ReactNode }) {
  const user = useAuth((s) => s.user);
  const loading = useAuth((s) => s.loading);
  const location = useLocation();

  if (loading) return <Loading />;
  if (!user) return <Navigate to="/login" state={{ from: location.pathname }} replace />;
  if (!roles.includes(user.role)) return <ForbiddenPage />;

  return <>{children}</>;
}

function RoleHome() {
  const user = useAuth((s) => s.user);
  const loading = useAuth((s) => s.loading);

  if (loading) return <Loading />;
  if (!user) return <Navigate to="/login" replace />;
  return <Navigate to={homeFor(user.role)} replace />;
}

export function AppRouter() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/" element={<RoleHome />} />

      {/* Рабочее место — вне общей навигации: оператор на смене не уходит из АРМ */}
      <Route
        path="/student/attempts/:attemptId/call"
        element={<Protected roles={['student']}><IncomingCallPage /></Protected>}
      />
      <Route
        path="/student/attempts/:attemptId/arm"
        element={<Protected roles={['student']}><Arm112Page /></Protected>}
      />

      <Route element={<Protected roles={['teacher', 'student', 'admin']}><Shell /></Protected>}>
        <Route path="/teacher" element={<Protected roles={['teacher']}><TeacherDashboard /></Protected>} />
        <Route path="/teacher/scenarios" element={<Protected roles={['teacher']}><ScenarioListPage /></Protected>} />
        <Route path="/teacher/scenarios/:scenarioId" element={<Protected roles={['teacher']}><ScenarioDetailPage /></Protected>} />
        <Route path="/teacher/lessons" element={<Protected roles={['teacher']}><LessonListPage /></Protected>} />
        <Route path="/teacher/lessons/:lessonId" element={<Protected roles={['teacher']}><LessonDetailPage /></Protected>} />
        <Route path="/teacher/attempts/:attemptId" element={<Protected roles={['teacher']}><AttemptReportPage /></Protected>} />

        <Route path="/student" element={<Protected roles={['student']}><StudentDashboard /></Protected>} />
        <Route path="/student/lessons/:lessonId" element={<Protected roles={['student']}><StudentLessonPage /></Protected>} />
        <Route path="/student/attempts/:attemptId/result" element={<Protected roles={['student']}><ResultPage /></Protected>} />

        <Route path="/admin" element={<Protected roles={['admin']}><AdminDashboard /></Protected>} />
        <Route path="/admin/users" element={<Protected roles={['admin']}><AdminUsersPage /></Protected>} />

        <Route path="/403" element={<ForbiddenPage />} />
        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  );
}
