/**
 * Голос заявителя синтезатором браузера — запасной путь, когда аудио ai-service
 * нет. Работает офлайн на голосах ОС; без русского голоса не звучит вовсе —
 * чужой голос с акцентом был бы хуже текста.
 *
 * @returns true — реплика отправлена синтезатору
 */
export function speakRussian(text: string): boolean {
  const synth = typeof window !== 'undefined' ? window.speechSynthesis : undefined;
  if (!synth || typeof SpeechSynthesisUtterance === 'undefined') return false;
  const voice = synth.getVoices().find((v) => v.lang.toLowerCase().startsWith('ru'));
  if (!voice) return false;
  synth.cancel();
  const u = new SpeechSynthesisUtterance(text);
  u.voice = voice;
  u.lang = voice.lang;
  synth.speak(u);
  return true;
}
