import { Suspense, lazy } from 'react';
import type { ReactNode } from 'react';
import { Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { homeFor, useAuth } from './auth';
import { Shell } from '../components/Shell';
import { Loading } from '../components/ui';
import { LoginPage } from '../pages/login/LoginPage';
import { ForbiddenPage, NotFoundPage } from '../pages/errors/ErrorPages';
import type { Role } from '../shared/types';

/*
 * Разделы грузятся по требованию: обучающемуся на рабочем месте не нужен код
 * кабинета преподавателя, аналитики и журналов администратора — а телефону в
 * локальной сети (ТЗ) не нужно качать их при входе.
 */
const TeacherDashboard = lazy(() => import('../pages/teacher/TeacherDashboard').then((m) => ({ default: m.TeacherDashboard })));
const ScenarioListPage = lazy(() => import('../pages/teacher/ScenarioListPage').then((m) => ({ default: m.ScenarioListPage })));
const ScenarioDetailPage = lazy(() => import('../pages/teacher/ScenarioDetailPage').then((m) => ({ default: m.ScenarioDetailPage })));
const LessonListPage = lazy(() => import('../pages/teacher/LessonListPage').then((m) => ({ default: m.LessonListPage })));
const LessonDetailPage = lazy(() => import('../pages/teacher/LessonDetailPage').then((m) => ({ default: m.LessonDetailPage })));
const AttemptReportPage = lazy(() => import('../pages/teacher/AttemptReportPage').then((m) => ({ default: m.AttemptReportPage })));
const StudentDashboard = lazy(() => import('../pages/student/StudentDashboard').then((m) => ({ default: m.StudentDashboard })));
const StudentLessonPage = lazy(() => import('../pages/student/StudentLessonPage').then((m) => ({ default: m.StudentLessonPage })));
const IncomingCallPage = lazy(() => import('../pages/student/IncomingCallPage').then((m) => ({ default: m.IncomingCallPage })));
const Arm112Page = lazy(() => import('../pages/student/Arm112Page').then((m) => ({ default: m.Arm112Page })));
const ResultPage = lazy(() => import('../pages/student/ResultPage').then((m) => ({ default: m.ResultPage })));
const AdminDashboard = lazy(() => import('../pages/admin/AdminDashboard').then((m) => ({ default: m.AdminDashboard })));
const AdminUsersPage = lazy(() => import('../pages/admin/AdminUsersPage').then((m) => ({ default: m.AdminUsersPage })));
const AnalyticsPage = lazy(() => import('../pages/analytics/AnalyticsPage').then((m) => ({ default: m.AnalyticsPage })));
const MaterialsPage = lazy(() => import('../pages/materials/MaterialsPage').then((m) => ({ default: m.MaterialsPage })));
const MaterialPage = lazy(() => import('../pages/materials/MaterialsPage').then((m) => ({ default: m.MaterialPage })));
const MyProgressPage = lazy(() => import('../pages/progress/ProgressPages').then((m) => ({ default: m.MyProgressPage })));
const StudentProgressPage = lazy(() => import('../pages/progress/ProgressPages').then((m) => ({ default: m.StudentProgressPage })));
const AdminAuditPage = lazy(() => import('../pages/admin/AdminAuditPage').then((m) => ({ default: m.AdminAuditPage })));
const AdminBackupsPage = lazy(() => import('../pages/admin/AdminBackupsPage').then((m) => ({ default: m.AdminBackupsPage })));
const AdminLogsPage = lazy(() => import('../pages/admin/AdminLogsPage').then((m) => ({ default: m.AdminLogsPage })));
const AdminSettingsPage = lazy(() => import('../pages/admin/AdminSettingsPage').then((m) => ({ default: m.AdminSettingsPage })));

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

  return <Suspense fallback={<Loading />}>{children}</Suspense>;
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
        <Route path="/teacher/analytics" element={<Protected roles={['teacher']}><AnalyticsPage /></Protected>} />
        <Route path="/teacher/students/:userId" element={<Protected roles={['teacher']}><StudentProgressPage /></Protected>} />

        <Route path="/student" element={<Protected roles={['student']}><StudentDashboard /></Protected>} />
        <Route path="/student/lessons/:lessonId" element={<Protected roles={['student']}><StudentLessonPage /></Protected>} />
        <Route path="/student/attempts/:attemptId/result" element={<Protected roles={['student']}><ResultPage /></Protected>} />
        <Route path="/student/progress" element={<Protected roles={['student']}><MyProgressPage /></Protected>} />

        <Route path="/admin" element={<Protected roles={['admin']}><AdminDashboard /></Protected>} />
        <Route path="/admin/users" element={<Protected roles={['admin']}><AdminUsersPage /></Protected>} />
        <Route path="/admin/analytics" element={<Protected roles={['admin']}><AnalyticsPage /></Protected>} />
        <Route path="/admin/audit" element={<Protected roles={['admin']}><AdminAuditPage /></Protected>} />
        <Route path="/admin/backups" element={<Protected roles={['admin']}><AdminBackupsPage /></Protected>} />
        <Route path="/admin/logs" element={<Protected roles={['admin']}><AdminLogsPage /></Protected>} />
        <Route path="/admin/settings" element={<Protected roles={['admin']}><AdminSettingsPage /></Protected>} />

        <Route path="/materials" element={<Protected roles={['teacher', 'student', 'admin']}><MaterialsPage /></Protected>} />
        <Route path="/materials/:materialId" element={<Protected roles={['teacher', 'student', 'admin']}><MaterialPage /></Protected>} />

        <Route path="/403" element={<ForbiddenPage />} />
        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  );
}
