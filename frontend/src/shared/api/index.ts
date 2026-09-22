/**
 * Точка подмены сервисного слоя.
 *
 * По умолчанию — mock: демонстрационный стенд работает без backend.
 * `VITE_USE_MOCKS=false` включает HTTP-реализацию по frontend.v1.yaml
 * (go-core, /api/v1; в dev — через прокси vite.config.ts).
 *
 * UI импортирует только `api` и типы из `./types` — менять компоненты не придётся.
 */

import { mockApi } from '../mocks/mockApi';
import { httpApi } from './httpApi';
import type { Api } from './types';

export const api: Api = import.meta.env.VITE_USE_MOCKS === 'false' ? httpApi : mockApi;

export type * from './types';
export { ApiRequestError, isApiError, hasErrorCode } from './error';
