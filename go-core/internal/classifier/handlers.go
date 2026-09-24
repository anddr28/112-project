package classifier

import (
	"log/slog"
	"net/http"
	"strings"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/reaction"
)

// Handlers — публичные ручки справочников (frontend.v1.yaml, tag classifier) и
// GET /reaction/transitions. Все маршруты — любой вошедший пользователь.
type Handlers struct {
	cat *Catalog
	log *slog.Logger
}

func NewHandlers(cat *Catalog, log *slog.Logger) *Handlers {
	if log == nil {
		log = slog.Default()
	}
	return &Handlers{cat: cat, log: log}
}

// Register — маршруты относительно /api/v1.
func (h *Handlers) Register(r *httpx.Router) {
	r.Handle("GET /classifier/types", httpx.Authenticated, h.types)
	r.Handle("GET /classifier/types/search", httpx.Authenticated, h.search)
	r.Handle("GET /classifier/featured", httpx.Authenticated, h.featured)
	r.Handle("GET /classifier/types/{typeId}/attributes", httpx.Authenticated, h.attributes)
	r.Handle("GET /classifier/types/{typeId}/reference", httpx.Authenticated, h.reference)
	r.Handle("GET /classifier/labels", httpx.Authenticated, h.labels)
	r.Handle("POST /classifier/resolve-services", httpx.Authenticated, h.resolveServices)
	r.Handle("GET /services", httpx.Authenticated, h.services)
	r.Handle("GET /address/suggest", httpx.Authenticated, h.suggest)
	r.Handle("GET /reaction/transitions", httpx.Authenticated, h.transitions)
}

// cacheControl — справочник у пользователя живёт минуту, дальше — условный запрос по ETag
// (304 без тела). private: ответы только для вошедших, общим кэшам их держать нельзя.
const cacheControl = "private, max-age=60"

// serveCached отдаёт предкодированный ответ с ETag; If-None-Match совпал -> 304.
func serveCached(w http.ResponseWriter, r *http.Request, c cached) error {
	hd := w.Header()
	hd.Set("ETag", c.etag)
	hd.Set("Cache-Control", cacheControl)
	if etagMatch(r.Header.Get("If-None-Match"), c.etag) {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	httpx.WriteRawJSON(w, http.StatusOK, c.body)
	return nil
}

// etagMatch — If-None-Match: список тегов через запятую, слабые (W/) сравниваются
// по значению (RFC 9110 §13.1.2 — слабое сравнение для GET), «*» — любой.
func etagMatch(header, etag string) bool {
	if header == "" {
		return false
	}
	for part := range strings.SplitSeq(header, ",") {
		t := strings.TrimSpace(part)
		if t == "*" {
			return true
		}
		t = strings.TrimPrefix(t, "W/")
		if t == etag {
			return true
		}
	}
	return false
}

func (h *Handlers) types(w http.ResponseWriter, r *http.Request) error {
	return serveCached(w, r, h.cat.s().typesBody)
}

func (h *Handlers) featured(w http.ResponseWriter, r *http.Request) error {
	return serveCached(w, r, h.cat.s().featuredBody)
}

func (h *Handlers) labels(w http.ResponseWriter, r *http.Request) error {
	return serveCached(w, r, h.cat.s().labelsBody)
}

func (h *Handlers) services(w http.ResponseWriter, r *http.Request) error {
	return serveCached(w, r, h.cat.s().servicesBody)
}

func (h *Handlers) search(w http.ResponseWriter, r *http.Request) error {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		return httpx.Validation("Введите текст для поиска типа происшествия", map[string]string{"q": "обязательно"})
	}
	limit := httpx.QueryInt(r, "limit", SearchDefaultLimit, 1, SearchMaxLimit)
	httpx.WriteRawJSON(w, http.StatusOK, h.cat.searchJSON(q, limit))
	return nil
}

func (h *Handlers) attributes(w http.ResponseWriter, r *http.Request) error {
	e := h.cat.s().lookupType(r.PathValue("typeId"))
	if e == nil {
		return httpx.NotFound("Тип происшествия не найден")
	}
	return serveCached(w, r, e.attrsBody)
}

// reference — справка по типу. Спека не предусматривает 404: неизвестный тип — {exists:false}
// (как мок: справки нет).
func (h *Handlers) reference(w http.ResponseWriter, r *http.Request) error {
	body := refMissing
	if e := h.cat.s().lookupType(r.PathValue("typeId")); e != nil {
		body = e.refBody
	}
	w.Header().Set("Cache-Control", cacheControl)
	httpx.WriteRawJSON(w, http.StatusOK, body)
	return nil
}

func (h *Handlers) resolveServices(w http.ResponseWriter, r *http.Request) error {
	var req public.ResolveServicesRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		return err
	}
	fields := map[string]string{}
	for _, id := range req.TypeIds {
		if strings.TrimSpace(id) == "" {
			fields["typeIds"] = "пустой идентификатор типа"
			break
		}
	}
	if req.AddressSource != nil && !req.AddressSource.Valid() {
		fields["addressSource"] = "неизвестный источник адреса"
	}
	for i := range req.Current {
		as := &req.Current[i]
		if strings.TrimSpace(as.Code) == "" {
			fields["current"] = "у службы нет кода"
			break
		}
		if !as.CurrentStatus.Valid() {
			fields["current"] = "неизвестный статус реагирования «" + string(as.CurrentStatus) + "»"
			break
		}
		if !as.Source.Valid() {
			fields["current"] = "неизвестный источник назначения службы"
			break
		}
	}
	if len(fields) > 0 {
		return httpx.Validation("Некорректный запрос определения служб", fields)
	}
	httpx.WriteJSON(w, http.StatusOK, h.cat.ResolveServices(req))
	return nil
}

func (h *Handlers) suggest(w http.ResponseWriter, r *http.Request) error {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < SuggestMinQuery {
		return httpx.Validation("Введите не меньше 2 символов адреса", map[string]string{"q": "не короче 2 символов"})
	}
	limit := httpx.QueryInt(r, "limit", SuggestDefaultLimit, 1, SuggestMaxLimit)
	httpx.WriteJSON(w, http.StatusOK, Suggest(q, limit))
	return nil
}

func (h *Handlers) transitions(w http.ResponseWriter, r *http.Request) error {
	qs := r.URL.Query()
	current := qs.Get("current")
	code := strings.TrimSpace(qs.Get("serviceCode"))
	fields := map[string]string{}
	if current == "" {
		fields["current"] = "обязательно"
	} else if !reaction.ValidStatus(current) {
		fields["current"] = "неизвестный статус реагирования"
	}
	if code == "" {
		fields["serviceCode"] = "обязательно"
	}
	if len(fields) > 0 {
		return httpx.Validation("Некорректные параметры запроса переходов статуса", fields)
	}
	w.Header().Set("Cache-Control", cacheControl)
	httpx.WriteJSON(w, http.StatusOK, reaction.AllowedNext(public.ReactionStatus(current), code))
	return nil
}
