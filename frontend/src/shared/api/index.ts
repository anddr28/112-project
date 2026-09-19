/**
 * Точка подмены сервисного слоя.
 *
 * Сейчас экспортируется mock. Когда появится `contracts/openapi/frontend.v1.yaml`,
 * здесь появится вторая реализация `httpApi` поверх fetch и переключение:
 *
 *   export const api: Api = import.meta.env.VITE_USE_MOCKS === 'false' ? httpApi : mockApi;
 *
 * UI импортирует только `api` и типы из `./types` — менять компоненты не придётся.
 */

import { mockApi } from '../mocks/mockApi';
import type { Api } from './types';

export const api: Api = mockApi;

export type * from './types';
