package reports

import (
	"bufio"
	"encoding/csv"
	"io"
	"strconv"
	"strings"
	"time"
)

// utf8BOM — без BOM Excel открывает UTF-8 CSV в cp1251 и русский текст превращается в «кракозябры».
const utf8BOM = "\xEF\xBB\xBF"

// writeCSV — таблица результатов для Excel в русской локали: UTF-8 с BOM, разделитель «;»,
// CRLF, десятичная запятая. Только строки — без сводки: CSV остаётся машиночитаемым
// (сводка есть в XLSX и PDF). Пишется потоково в w.
func writeCSV(w io.Writer, rep *report) error {
	bw := bufio.NewWriterSize(w, 32<<10)
	if _, err := bw.WriteString(utf8BOM); err != nil {
		return err
	}
	cw := csv.NewWriter(bw)
	cw.Comma = ';'
	cw.UseCRLF = true

	rec := make([]string, len(columns))
	for i, c := range columns {
		rec[i] = c.Title
	}
	if err := cw.Write(rec); err != nil {
		return err
	}
	for i := range rep.Rows {
		r := &rep.Rows[i]
		for j, c := range columns {
			rec[j] = csvValue(c.Kind, c.value(i, r), rep.Loc)
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	return bw.Flush()
}

func csvValue(kind colKind, v any, loc *time.Location) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return csvText(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if kind == kSeconds {
			return fmtDecimal(x, 1)
		}
		return fmtDecimal(x, 0)
	case bool:
		if x {
			return "да"
		}
		return "нет"
	case time.Time:
		return x.In(loc).Format(layoutDateTime)
	}
	return ""
}

// csvText — защита от CSV/formula injection (OWASP): текст из ввода пользователей (ФИО,
// причина корректировки, названия) не должен начинаться с символа, который Excel примет
// за формулу. Перевод строк внутри ячейки — пробел: таблица читается построчно.
func csvText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	if s != "" && strings.ContainsRune("=+-@", rune(s[0])) {
		return "'" + s
	}
	return s
}
