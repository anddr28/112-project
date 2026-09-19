import type { User } from '../types';

/** Фамилия Имя Отчество полностью. */
export function fullName(u: User): string {
  return [u.lastName, u.firstName, u.middleName].filter(Boolean).join(' ');
}

/** Фамилия И. О. — для шапки и подписей под действиями. */
export function shortName(u: User): string {
  const i = u.firstName ? `${u.firstName[0]}.` : '';
  const m = u.middleName ? `${u.middleName[0]}.` : '';
  return `${u.lastName} ${i}${m}`.trim();
}
