/**
 * Сохранить полученный файл на устройство. Нужен, когда файл пришёл запросом
 * (а не ссылкой): так отказ сервера можно показать текстом, а не скачать.
 */
export function saveBlob(blob: Blob, fileName: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = fileName;
  document.body.append(a);
  a.click();
  a.remove();
  // Отзыв сразу после click() в части браузеров обрывает скачивание.
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
}

/** JSON в файл (выгрузка пакета сценариев). */
export function saveJson(value: unknown, fileName: string): void {
  saveBlob(new Blob([JSON.stringify(value, null, 2)], { type: 'application/json' }), fileName);
}
