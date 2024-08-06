package api

import (
	"github.com/Lucas0204/go-ask-me-anything/internal/store/pgstore"
	"github.com/go-chi/chi/v5"
	"net/http"
)

type apiHandler struct {
	q *pgstore.Queries
	r *chi.Mux
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}

func NewHandler(q *pgstore.Queries) http.Handler {
	handler := apiHandler{
		q: q,
	}

	r := chi.NewRouter()
	handler.r = r
	return handler
}
