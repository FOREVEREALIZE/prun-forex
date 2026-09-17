package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type apiOrder struct {
	ID        int64     `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Amount    int64     `json:"amount"`
	Remaining int64     `json:"remaining"`
	Filled    int64     `json:"filled"`
	Status    string    `json:"status"`
	Owner     string    `json:"owner"`         // "[CORP] User | CODE"
	OwnerCode string    `json:"owner_company"` // company code
	OwnerTag  string    `json:"owner_discord"` // Discord username
	CreatedAt time.Time `json:"created_at"`
	Fills     []apiFill `json:"fills,omitempty"`
}

type apiFill struct {
	Amount     int64     `json:"amount"`
	Filler     string    `json:"filler"`
	FillerCode string    `json:"filler_company"`
	FillerTag  string    `json:"filler_discord"`
	CreatedAt  time.Time `json:"created_at"`
}

func toAPIOrder(o Order) apiOrder {
	a := apiOrder{ID: o.ID, From: o.From, To: o.To, Amount: o.Amount, Remaining: o.Remaining,
		Filled: o.Filled(), Status: o.Status, Owner: o.Owner.Name(), OwnerCode: o.Owner.CompanyCode, OwnerTag: o.Owner.Handle, CreatedAt: o.CreatedAt.UTC()}
	for _, f := range o.Fills {
		a.Fills = append(a.Fills, apiFill{Amount: f.Amount, Filler: f.Filler.Name(), FillerCode: f.Filler.CompanyCode, FillerTag: f.Filler.Handle, CreatedAt: f.CreatedAt.UTC()})
	}
	return a
}

// GET /api/orders?status=open|filled|cancelled|all&from=AIC&to=NCC&limit=100
func (s *Server) handleAPIOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := OrderFilter{Status: "open", Limit: 100}
	switch st := q.Get("status"); st {
	case "", "open":
	case "filled", "cancelled":
		f.Status = st
	case "all":
		f.Status = ""
	default:
		apiError(w, http.StatusBadRequest, "status must be open, filled, cancelled or all")
		return
	}
	for _, p := range []struct {
		key string
		dst *string
	}{{"from", &f.From}, {"to", &f.To}} {
		if v := strings.ToUpper(q.Get(p.key)); v != "" {
			if !validCurrency(v) {
				apiError(w, http.StatusBadRequest, p.key+" must be one of "+strings.Join(Currencies, ", "))
				return
			}
			*p.dst = v
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			apiError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		f.Limit = n
	}
	orders, err := s.store.ListOrders(r.Context(), f)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]apiOrder, 0, len(orders))
	for _, o := range orders {
		out = append(out, toAPIOrder(o))
	}
	writeJSON(w, map[string]any{"orders": out})
}

func (s *Server) handleAPIOrder(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		apiError(w, http.StatusNotFound, "order not found")
		return
	}
	o, err := s.store.OrderByID(r.Context(), id)
	if err == ErrNotFound {
		apiError(w, http.StatusNotFound, "order not found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, toAPIOrder(*o))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
