from ai_service.gen import api_models as am
from ai_service.tasks.speech_metrics import count_fillers, fillers_per_100, speech_metrics

FILLERS = ["ну", "эээ", "как бы", "это самое", "вот"]


def t(n, who, text, at=None, dur=None, conf=None):
    return am.DialogueTurn(turn_no=n, speaker=who, text=text, at_ms=at, audio_duration_ms=dur, confidence=conf)


def test_count_fillers_bigrams_not_double_counted():
    c = count_fillers(["Ну как бы это самое, вот, ну"], FILLERS)
    assert c == {"ну": 2, "как бы": 1, "это самое": 1, "вот": 1}


def test_speech_metrics():
    tr = [t(1, "caller", "Алло!", 0, 1000), t(2, "operator", "Ну, слушаю вас", 3000, 2000, 0.9),
          t(3, "caller", "Горит!", 6000, 1000), t(4, "operator", "эээ адрес", 9000, 1000, 0.4)]
    m = speech_metrics(tr, FILLERS, floor=0.6)
    assert m["operator_turns"] == 2 and m["operator_words"] == 5 and m["operator_talk_ms"] == 3000
    assert m["words_per_min"] == 100.0
    assert m["filler_count"] == 2 and m["low_confidence_turns"] == 1
    assert m["avg_response_ms"] == 2000 and m["max_response_ms"] == 2000
    assert fillers_per_100(m) == 40.0


def test_speech_metrics_text_mode():
    m = speech_metrics([t(1, "caller", "Алло"), t(2, "operator", "Слушаю")], FILLERS)
    assert "words_per_min" not in m and "avg_response_ms" not in m and m["operator_words"] == 1
