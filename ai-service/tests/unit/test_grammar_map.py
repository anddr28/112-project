from ai_service.tasks.grammar import _is_domain_word, severity_of


def test_severity_mapping():
    assert severity_of("misspelling", "TYPOS") == "error"
    assert severity_of("grammar", "GRAMMAR") == "error"
    assert severity_of("typographical", "TYPOGRAPHY") == "warning"
    assert severity_of("uncategorized", "PUNCTUATION") == "warning"
    assert severity_of("style", "STYLE") == "style"


def test_domain_words_not_errors():
    d = {"дтп", "жкх"}
    assert _is_domain_word("ДТП", d) and _is_domain_word("Ленина", d) and _is_domain_word("14а", d)
    assert not _is_domain_word("пажар", d)
