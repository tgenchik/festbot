package devchat

import (
	"context"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

//go:embed ui.html
var uiHTML []byte

// Action — сценарное действие с панели (засеять данные, провести розыгрыш...).
// Возвращает текст, который страница покажет как результат.
type Action func(ctx context.Context) (string, error)

type Server struct {
	store   *Store
	handle  func(context.Context, model.Update)
	actions map[string]Action
	log     *slog.Logger
}

func NewServer(store *Store, handle func(context.Context, model.Update), actions map[string]Action, log *slog.Logger) *Server {
	return &Server{store: store, handle: handle, actions: actions, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(uiHTML)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /dev/api/chat", s.chat)
	mux.HandleFunc("POST /dev/api/start", s.start)
	mux.HandleFunc("POST /dev/api/text", s.text)
	mux.HandleFunc("POST /dev/api/callback", s.callback)
	mux.HandleFunc("POST /dev/api/subscribe", s.subscribe)
	mux.HandleFunc("POST /dev/api/clear", s.clear)
	mux.HandleFunc("POST /dev/api/action/{name}", s.action)
	return mux
}

type userReq struct {
	User      int64  `json:"user"`
	Name      string `json:"name"`
	Text      string `json:"text"`
	MessageID int64  `json:"message_id"`
	Payload   string `json:"payload"`
	On        bool   `json:"on"`
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	userID, err := strconv.ParseInt(r.URL.Query().Get("user"), 10, 64)
	if err != nil {
		http.Error(w, "user is required", http.StatusBadRequest)
		return
	}
	ver, msgs, err := s.store.Chat(r.Context(), userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	sub, err := s.store.IsSubscribed(r.Context(), userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"version": ver, "subscribed": sub, "messages": msgs})
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decode(w, r)
	if !ok {
		return
	}
	if _, err := s.store.Append(r.Context(), req.User, ChatMessage{From: "user", Text: "Начать"}); err != nil {
		s.fail(w, err)
		return
	}
	u := model.Update{UpdateType: model.UpdateBotStarted, UserID: req.User, User: &model.User{
		UserID: req.User, FirstName: firstName(req.Name), LastName: lastName(req.Name),
	}}
	s.dispatch(w, r, u)
}

func (s *Server) text(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decode(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		http.Error(w, "text is empty", http.StatusBadRequest)
		return
	}
	if _, err := s.store.Append(r.Context(), req.User, ChatMessage{From: "user", Text: req.Text}); err != nil {
		s.fail(w, err)
		return
	}
	u := model.Update{
		UpdateType: model.UpdateMessageCreated,
		UserID:     req.User,
		Message: &model.MessageUpdate{
			Sender: model.Sender{UserID: req.User, FirstName: firstName(req.Name), LastName: lastName(req.Name)},
			Body:   model.MessageBody{Text: req.Text},
		},
	}
	s.dispatch(w, r, u)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decode(w, r)
	if !ok {
		return
	}
	u := model.Update{
		UpdateType: model.UpdateMessageCallback,
		UserID:     req.User,
		Callback: &model.Callback{
			CallbackID: callbackID(req.User, req.MessageID),
			Payload:    req.Payload,
			User:       model.User{UserID: req.User, FirstName: firstName(req.Name), LastName: lastName(req.Name)},
		},
	}
	s.dispatch(w, r, u)
}

func (s *Server) subscribe(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decode(w, r)
	if !ok {
		return
	}
	if err := s.store.SetSubscribed(r.Context(), req.User, req.On); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) clear(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decode(w, r)
	if !ok {
		return
	}
	if err := s.store.ClearChat(r.Context(), req.User); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	act, ok := s.actions[r.PathValue("name")]
	if !ok {
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	msg, err := act(ctx)
	if err != nil {
		// Ошибки сценария (уже разыграно, нет участников) показываем на странице как есть.
		writeJSON(w, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": msg})
}

// dispatch прогоняет апдейт через настоящий обработчик бота.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, u model.Update) {
	s.handle(r.Context(), u)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request) (userReq, bool) {
	var req userReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.User <= 0 {
		http.Error(w, "bad request: user id is required", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("devchat", "err", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func firstName(full string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(full), " ")
	return first
}

func lastName(full string) string {
	_, last, _ := strings.Cut(strings.TrimSpace(full), " ")
	return strings.TrimSpace(last)
}
