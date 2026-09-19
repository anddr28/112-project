import { Link } from 'react-router-dom';
import { EmptyState } from '../../components/ui';

export function ForbiddenPage({ text }: { text?: string } = {}) {
  return (
    <EmptyState
      title="Недостаточно прав"
      text={
        text ??
        'Этот раздел доступен другой роли. Ограничение проверяется и на сервере — обойти его через интерфейс нельзя.'
      }
      action={<Link className="btn" to="/">На главную</Link>}
    />
  );
}

export function NotFoundPage() {
  return (
    <EmptyState
      title="Страница не найдена"
      text="Проверьте адрес или вернитесь к своему разделу."
      action={<Link className="btn" to="/">На главную</Link>}
    />
  );
}
