// Package admin — HTTP API для команды акции: партнеры, расписание, промокоды,
// розыгрыши с подтверждением, выгрузки, рассылки и статистика.
// Закрыт Basic Auth; веб-интерфейс можно надстроить поверх этих ручек.
package admin

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgenchik/festbot/internal/broadcast"
	"github.com/tgenchik/festbot/internal/campaign"
	"github.com/tgenchik/festbot/internal/draws"
	"github.com/tgenchik/festbot/internal/messenger"
	"github.com/tgenchik/festbot/internal/prizes"
	"github.com/tgenchik/festbot/internal/promo"
	"github.com/tgenchik/festbot/internal/stats"
)

type Deps struct {
	Log       *slog.Logger
	DB        *pgxpool.Pool
	Campaign  *campaign.Repo
	Promo     *promo.Service
	Draws     *draws.Service
	Prizes    *prizes.Service
	Broadcast *broadcast.Service
	Location  *time.Location
	User      string
	Password  string
}

func Router(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, basicAuth(d.User, d.Password))
	h := handlers{d}

	r.Get("/api/stats", h.stats)
	r.Post("/api/partners", h.createPartner)
	r.Post("/api/partners/{id}/promocodes", h.loadPromo) // тело: коды по одному в строке
	r.Put("/api/days", h.upsertDay)

	r.Post("/api/draws/daily/{date}", h.runDaily) // date = 2026-09-01
	r.Post("/api/draws/final", h.runFinal)
	r.Get("/api/draws/{id}", h.getDraw)
	r.Post("/api/draws/{id}/confirm", h.confirmDraw)
	r.Post("/api/draws/{id}/cancel", h.cancelDraw)
	r.Get("/api/draws/{id}/participants.csv", h.exportDraw)
	r.Get("/api/draws/{id}/claims.csv", h.exportClaims)

	r.Post("/api/broadcasts", h.createBroadcast)
	return r
}

type handlers struct{ Deps }

func (h handlers) stats(w http.ResponseWriter, r *http.Request) {
	rep, err := stats.Build(r.Context(), h.DB, h.Location)
	h.reply(w, rep, err)
}

func (h handlers) createPartner(w http.ResponseWriter, r *http.Request) {
	var p campaign.Partner
	if !decode(w, r, &p) {
		return
	}
	id, err := h.Campaign.CreatePartner(r.Context(), p)
	h.reply(w, map[string]int64{"id": id}, err)
}

func (h handlers) upsertDay(w http.ResponseWriter, r *http.Request) {
	var in struct {
		campaign.DayInput
		Date string `json:"date"`
	}
	if !decode(w, r, &in) {
		return
	}
	date, err := time.Parse(time.DateOnly, in.Date)
	if err != nil {
		http.Error(w, "date: ожидается YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	in.DayInput.Date = date
	if in.WinnersCount == 0 {
		in.WinnersCount = 1
	}
	h.reply(w, map[string]bool{"ok": true}, h.Campaign.UpsertDay(r.Context(), in.DayInput))
}

func (h handlers) loadPromo(w http.ResponseWriter, r *http.Request) {
	partnerID, ok := idParam(w, r)
	if !ok {
		return
	}
	var codes []string
	sc := bufio.NewScanner(r.Body)
	for sc.Scan() {
		if c := strings.TrimSpace(strings.Trim(sc.Text(), `",;`)); c != "" {
			codes = append(codes, c)
		}
	}
	if err := sc.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	added, err := h.Promo.Load(r.Context(), partnerID, codes)
	h.reply(w, map[string]int64{"received": int64(len(codes)), "added": added}, err)
}

func (h handlers) runDaily(w http.ResponseWriter, r *http.Request) {
	date, err := time.Parse(time.DateOnly, chi.URLParam(r, "date"))
	if err != nil {
		http.Error(w, "date: ожидается YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	id, err := h.Draws.RunDaily(r.Context(), date)
	h.reply(w, map[string]int64{"draw_id": id}, err)
}

func (h handlers) runFinal(w http.ResponseWriter, r *http.Request) {
	id, err := h.Draws.RunFinal(r.Context())
	h.reply(w, map[string]int64{"draw_id": id}, err)
}

func (h handlers) getDraw(w http.ResponseWriter, r *http.Request) {
	if id, ok := idParam(w, r); ok {
		d, err := h.Draws.Get(r.Context(), id)
		h.reply(w, d, err)
	}
}

func (h handlers) confirmDraw(w http.ResponseWriter, r *http.Request) {
	if id, ok := idParam(w, r); ok {
		user, _, _ := r.BasicAuth()
		h.reply(w, map[string]bool{"ok": true}, h.Draws.Confirm(r.Context(), id, user))
	}
}

func (h handlers) cancelDraw(w http.ResponseWriter, r *http.Request) {
	if id, ok := idParam(w, r); ok {
		h.reply(w, map[string]bool{"ok": true}, h.Draws.Cancel(r.Context(), id))
	}
}

func (h handlers) exportDraw(w http.ResponseWriter, r *http.Request) {
	if id, ok := idParam(w, r); ok {
		csvHeaders(w, fmt.Sprintf("draw-%d-participants.csv", id))
		if err := h.Draws.ExportCSV(r.Context(), id, w); err != nil {
			h.Log.Error("export draw", "err", err)
		}
	}
}

func (h handlers) exportClaims(w http.ResponseWriter, r *http.Request) {
	if id, ok := idParam(w, r); ok {
		csvHeaders(w, fmt.Sprintf("draw-%d-claims.csv", id))
		if err := h.Prizes.ExportCSV(r.Context(), id, w); err != nil {
			h.Log.Error("export claims", "err", err)
		}
	}
}

func (h handlers) createBroadcast(w http.ResponseWriter, r *http.Request) {
	var msg messenger.Message
	if !decode(w, r, &msg) {
		return
	}
	if strings.TrimSpace(msg.Text) == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	id, err := h.Broadcast.Create(r.Context(), msg)
	h.reply(w, map[string]int64{"broadcast_id": id}, err)
}

func (h handlers) reply(w http.ResponseWriter, v any, err error) {
	switch {
	case errors.Is(err, draws.ErrAlreadyDrawn), errors.Is(err, draws.ErrBadStatus):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errors.Is(err, draws.ErrNoEntries), errors.Is(err, campaign.ErrNoDay):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	case err != nil:
		h.Log.Error("admin", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func csvHeaders(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write([]byte("\xEF\xBB\xBF")) // BOM, чтобы Excel открыл кириллицу
}

func basicAuth(user, pass string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || pass == "" ||
				subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="festbot"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
