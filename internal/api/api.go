package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Lucas0204/go-ask-me-anything/internal/store/pgstore"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"net/http"
	"sync"
)

type apiHandler struct {
	q           *pgstore.Queries
	r           *chi.Mux
	wsUpgrader  websocket.Upgrader
	subscribers map[string]map[*websocket.Conn]context.CancelFunc
	subsMutex   *sync.Mutex
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}

func NewHandler(q *pgstore.Queries) http.Handler {
	handler := apiHandler{
		q: q,
		wsUpgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		subscribers: make(map[string]map[*websocket.Conn]context.CancelFunc),
		subsMutex:   &sync.Mutex{},
	}

	r := chi.NewRouter()

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)

	r.Get("/subscribe/{room_id}", handler.handleSubscribeRoom)

	r.Route("/api", func(r chi.Router) {
		r.Route("/rooms", func(r chi.Router) {
			r.Post("/", handler.handleCreateRoom)
			r.Get("/", handler.handleGetRooms)

			r.Route("/{room_id}/messages", func(r chi.Router) {
				r.Post("/", handler.handleCreateMessage)
				r.Get("/", handler.handleGetRoomMessages)

				r.Route("/{message_id}", func(r chi.Router) {
					r.Get("/", handler.handleGetRoomMessage)
					r.Patch("/react", handler.handleReactionToMessage)
					r.Delete("/react", handler.handleRemoveReactionFromMessage)
					r.Patch("/answer", handler.handleMarkMessageAsAnswered)
				})
			})
		})
	})

	handler.r = r
	return handler
}

func (h apiHandler) handleSubscribeRoom(writer http.ResponseWriter, request *http.Request) {
	rawRoomId := chi.URLParam(request, "room_id")
	roomId, err := uuid.Parse(rawRoomId)
	if err != nil {
		http.Error(writer, "invalid room id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(request.Context(), roomId)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(writer, "room not found", http.StatusNotFound)
			return
		}

		http.Error(writer, "something went wrong", http.StatusInternalServerError)
		return
	}

	connection, err := h.wsUpgrader.Upgrade(writer, request, nil)
	if err != nil {
		slog.Warn("failed to upgrade connection to websocket", "error", err)
		http.Error(writer, "failed to upgrade connection to websocket", http.StatusInternalServerError)
		return
	}

	defer connection.Close()

	ctx, cancel := context.WithCancel(request.Context())

	h.subsMutex.Lock()
	if _, ok := h.subscribers[rawRoomId]; !ok {
		h.subscribers[rawRoomId] = make(map[*websocket.Conn]context.CancelFunc)
	}
	h.subscribers[rawRoomId][connection] = cancel
	h.subsMutex.Unlock()

	slog.Info("new client connected", "room_id", rawRoomId, "client_ip", request.RemoteAddr)

	<-ctx.Done()

	h.subsMutex.Lock()
	delete(h.subscribers[rawRoomId], connection)
	h.subsMutex.Unlock()
}

func (h apiHandler) handleCreateRoom(writer http.ResponseWriter, request *http.Request) {
	type _body struct {
		Theme string `json:"theme"`
	}
	var body _body
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, "invalid json", http.StatusBadRequest)
		return
	}

	roomId, err := h.q.InsertRoom(request.Context(), body.Theme)
	if err != nil {
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	data, _ := json.Marshal(response{ID: roomId.String()})
	writer.Header().Set("Content-Type", "application/json")
	_, err = writer.Write(data)
	if err != nil {
		slog.Error("failed to write response data", "room_id", roomId.String())
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
	}
}

func (h apiHandler) handleGetRooms(writer http.ResponseWriter, request *http.Request) {
	rooms, err := h.q.GetRooms(request.Context())
	if err != nil {
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		Rooms []pgstore.Room `json:"rooms"`
	}
	data, _ := json.Marshal(response{Rooms: rooms})
	writer.Header().Set("Content-Type", "application/json")
	_, err = writer.Write(data)
	if err != nil {
		slog.Error("failed to write response data to return rooms list")
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
	}
}

func (h apiHandler) handleCreateMessage(writer http.ResponseWriter, request *http.Request) {
	rawRoomId := chi.URLParam(request, "room_id")
	roomId, err := uuid.Parse(rawRoomId)
	if err != nil {
		http.Error(writer, "invalid room id", http.StatusBadRequest)
		return
	}

	type _body struct {
		Message string `json:"message"`
	}
	var body _body
	if err = json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, "invalid json", http.StatusBadRequest)
		return
	}

	var insertMessageParams = pgstore.InsertMessageParams{RoomID: roomId, Message: body.Message}
	messageId, err := h.q.InsertMessage(request.Context(), insertMessageParams)

	if err != nil {
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		MessageID uuid.UUID `json:"messageId"`
	}

	data, _ := json.Marshal(response{MessageID: messageId})
	writer.Header().Set("Content-Type", "application/json")
	_, err = writer.Write(data)
	if err != nil {
		slog.Error("failed to write response data to return message ID")
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
	}
}

func (h apiHandler) handleGetRoomMessages(writer http.ResponseWriter, request *http.Request) {
	rawRoomId := chi.URLParam(request, "room_id")
	roomId, err := uuid.Parse(rawRoomId)
	if err != nil {
		http.Error(writer, "invalid room id", http.StatusBadRequest)
		return
	}

	messages, err := h.q.GetRoomMessages(request.Context(), roomId)
	if err != nil {
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		Messages []pgstore.Message `json:"messages"`
	}

	data, _ := json.Marshal(response{Messages: messages})
	writer.Header().Set("Content-Type", "application/json")
	_, err = writer.Write(data)
	if err != nil {
		slog.Error("failed to write response data", "room_id", roomId.String())
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
	}
}

func (h apiHandler) handleGetRoomMessage(writer http.ResponseWriter, request *http.Request) {
	rawRoomId := chi.URLParam(request, "room_id")
	_, err := uuid.Parse(rawRoomId)
	if err != nil {
		http.Error(writer, "invalid room id", http.StatusBadRequest)
		return
	}

	rawMessageId := chi.URLParam(request, "message_id")
	messageId, err := uuid.Parse(rawMessageId)
	if err != nil {
		http.Error(writer, "invalid message id", http.StatusBadRequest)
		return
	}

	message, err := h.q.GetMessage(request.Context(), messageId)

	type response struct {
		Message pgstore.Message `json:"message"`
	}

	data, _ := json.Marshal(response{Message: message})
	writer.Header().Set("Content-Type", "application/json")
	_, err = writer.Write(data)
	if err != nil {
		slog.Error("failed to write response data", "message_id", messageId.String())
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
	}
}

func (h apiHandler) handleReactionToMessage(writer http.ResponseWriter, request *http.Request) {
	rawRoomId := chi.URLParam(request, "room_id")
	_, err := uuid.Parse(rawRoomId)
	if err != nil {
		http.Error(writer, "invalid room id", http.StatusBadRequest)
		return
	}

	rawMessageId := chi.URLParam(request, "message_id")
	messageId, err := uuid.Parse(rawMessageId)
	if err != nil {
		http.Error(writer, "invalid message id", http.StatusBadRequest)
		return
	}

	count, err := h.q.ReactToMessage(request.Context(), messageId)
	if err != nil {
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ReactionCount int64 `json:"reactionCount"`
	}

	data, _ := json.Marshal(response{ReactionCount: count})
	writer.Header().Set("Content-Type", "application/json")
	_, err = writer.Write(data)
	if err != nil {
		slog.Error("failed to write response data to return reaction count")
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
	}
}

func (h apiHandler) handleRemoveReactionFromMessage(writer http.ResponseWriter, request *http.Request) {
	rawRoomId := chi.URLParam(request, "room_id")
	_, err := uuid.Parse(rawRoomId)
	if err != nil {
		http.Error(writer, "invalid room id", http.StatusBadRequest)
		return
	}

	rawMessageId := chi.URLParam(request, "message_id")
	messageId, err := uuid.Parse(rawMessageId)
	if err != nil {
		http.Error(writer, "invalid message id", http.StatusBadRequest)
		return
	}

	count, err := h.q.RemoveReactionFromMessage(request.Context(), messageId)
	if err != nil {
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ReactionCount int64 `json:"reactionCount"`
	}

	data, _ := json.Marshal(response{ReactionCount: count})
	writer.Header().Set("Content-Type", "application/json")
	_, err = writer.Write(data)
	if err != nil {
		slog.Error("failed to write response data to return reaction count")
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
	}
}

func (h apiHandler) handleMarkMessageAsAnswered(writer http.ResponseWriter, request *http.Request) {
	rawRoomId := chi.URLParam(request, "room_id")
	_, err := uuid.Parse(rawRoomId)
	if err != nil {
		http.Error(writer, "invalid room id", http.StatusBadRequest)
		return
	}

	rawMessageId := chi.URLParam(request, "message_id")
	messageId, err := uuid.Parse(rawMessageId)
	if err != nil {
		http.Error(writer, "invalid message id", http.StatusBadRequest)
		return
	}

	err = h.q.MarkMessageAsAnswered(request.Context(), messageId)
	if err != nil {
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
	}
}
