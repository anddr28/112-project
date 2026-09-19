let counter = 0;

/** Короткий идентификатор для mock-сущностей и React-ключей. */
export function uid(prefix = 'id'): string {
  counter += 1;
  return `${prefix}-${Date.now().toString(36)}-${counter.toString(36)}`;
}
