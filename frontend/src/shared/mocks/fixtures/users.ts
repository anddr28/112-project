/** Учебные учётные записи. TODO(backend) GAP-19: пользовательского auth в контракте нет. */

import type { User } from '../../types';

export const USERS: User[] = [
  {
    id: 'u-teacher',
    login: 'teacher',
    role: 'teacher',
    lastName: 'Ковалёва',
    firstName: 'Ирина',
    middleName: 'Сергеевна',
    status: 'active',
  },
  {
    id: 'u-student',
    login: 'student',
    role: 'student',
    lastName: 'Рожкова',
    firstName: 'Ольга',
    middleName: 'Ивановна',
    serviceId: 'svc-zhkh',
    serviceName: 'ДДС ЖКХ',
    status: 'active',
    workstation: 'АРМ 4',
    operatorNo: 'оп. 227',
  },
  {
    id: 'u-student-2',
    login: 'student2',
    role: 'student',
    lastName: 'Никитин',
    firstName: 'Павел',
    middleName: 'Андреевич',
    serviceId: 'svc-zhkh',
    serviceName: 'ДДС ЖКХ',
    status: 'active',
    workstation: 'АРМ 7',
    operatorNo: 'оп. 315',
  },
  {
    id: 'u-admin',
    login: 'admin',
    role: 'admin',
    lastName: 'Глущенко',
    firstName: 'Олег',
    middleName: 'Игоревич',
    status: 'active',
  },
];

export const DEMO_ACCOUNTS: Array<{ login: string; password: string; role: User['role']; label: string }> = [
  { login: 'teacher', password: 'teacher', role: 'teacher', label: 'Преподаватель' },
  { login: 'student', password: 'student', role: 'student', label: 'Обучающийся' },
  { login: 'admin', password: 'admin', role: 'admin', label: 'Администратор' },
];
