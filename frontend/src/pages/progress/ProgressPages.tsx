import { Link, useParams } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { useAuth } from '../../app/auth';
import { ErrorState, Loading } from '../../components/ui';
import { CertificateButton, ProgressView } from '../../features/progress/ProgressView';

/** «Мой прогресс» — обучающийся видит только себя (сервер проверяет то же). */
export function MyProgressPage() {
  const userId = useAuth((s) => s.user?.id) ?? '';
  return <Progress userId={userId} title="Мой прогресс" sub="Опыт, уровень и результаты по всем занятиям." />;
}

/** Прогресс обучающегося глазами преподавателя: из занятия, разбора попытки или аналитики. */
export function StudentProgressPage() {
  const { userId = '' } = useParams();
  return <Progress userId={userId} back teacher />;
}

function Progress({ userId, title, sub, back, teacher }: { userId: string; title?: string; sub?: string; back?: boolean; teacher?: boolean }) {
  const progress = useAsync(() => api.users.progress(userId), [userId]);

  if (progress.loading) return <Loading />;
  if (progress.error) return <ErrorState text={progress.error} onRetry={progress.reload} />;
  if (!progress.data) return <ErrorState text="Прогресс не найден" />;
  const p = progress.data;

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          {back && (
            <div className="row row--tight" style={{ marginBottom: 6 }}>
              <Link to="/teacher/analytics" className="small">← Аналитика группы</Link>
            </div>
          )}
          <h1>{title ?? p.name}</h1>
          <div className="page-head__sub">{sub ?? 'Прогресс обучающегося: уровень, результаты, типичные ошибки.'}</div>
        </div>
        <div className="page-head__actions">
          {teacher && (
            <Link className="btn" to={`/teacher/analytics?studentId=${encodeURIComponent(userId)}`}>Аналитика по обучающемуся</Link>
          )}
          <CertificateButton userId={userId} disabled={p.attemptsDone === 0} />
        </div>
      </div>
      <ProgressView progress={p} />
    </>
  );
}
