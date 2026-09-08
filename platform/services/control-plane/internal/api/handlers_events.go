// Event ingestion + processing + traces + player state handlers.
package api

import (
	"net/http"
	"strings"

	"universalengagement/control-plane/internal/db"
	"universalengagement/control-plane/internal/eventing"
	"universalengagement/control-plane/internal/player"
	"universalengagement/control-plane/internal/processing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Events (§10 gateway + §4 processing)
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) IngestEvents(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Events []*eventing.CanonicalEvent `json:"events"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Events) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "validation_failed", "message": "events: at least one event is required"},
		})
		return
	}
	if len(req.Events) > 500 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error": map[string]any{"code": "batch_too_large", "message": "max 500 events per batch"}},
		)
		return
	}
	projectID := pathID(r, "projectId")
	envID := pathID(r, "environmentId")

	// Path scope overrides: events carry their own ids but must match the
	// route scope (tenant isolation).
	for _, e := range req.Events {
		e.ProjectID = projectID
		e.EnvironmentID = envID
		if e.ActorID == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": map[string]any{"code": "validation_failed", "message": "actor_id: each event requires an actor (the user it happened to)"},
			})
			return
		}
		if err := e.Normalize(); err != nil {
			writeErr(w, err)
			return
		}
	}

	results, err := eventing.IngestBatch(r.Context(), s.Pool, req.Events)
	if err != nil {
		writeErr(w, err)
		return
	}
	inserted := 0
	duplicated := 0
	for _, res := range results {
		switch res.Status {
		case "inserted":
			inserted++
		case "duplicate":
			duplicated++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results":    results,
		"inserted":   inserted,
		"duplicates": duplicated,
	})
}

func (s *Server) ProcessEvent(w http.ResponseWriter, r *http.Request) {
	deps := processing.Deps{Pool: s.Pool, Engine: s.EngineClient()}
	res, err := processing.ProcessEvent(r.Context(), deps, pathID(r, "projectId"), pathID(r, "environmentId"), pathID(r, "eventId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	status := http.StatusOK
	if res.Failed {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, res)
}

func (s *Server) ProcessBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EventIDs []string `json:"event_ids"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	deps := processing.Deps{Pool: s.Pool, Engine: s.EngineClient()}
	results := []*processing.Result{}
	for _, id := range req.EventIDs {
		res, err := processing.ProcessEvent(r.Context(), deps, pathID(r, "projectId"), pathID(r, "environmentId"), id)
		if err != nil {
			results = append(results, &processing.Result{EventID: id, Failed: true, Error: err.Error()})
			continue
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "count": len(results)})
}

func (s *Server) GetEvent(w http.ResponseWriter, r *http.Request) {
	ev, err := eventing.GetEvent(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"), pathID(r, "eventId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) ListEvents(w http.ResponseWriter, r *http.Request) {
	f := eventing.ListFilter{
		EventType: queryStr(r, "type", ""),
		ActorID:   queryStr(r, "actor_id", ""),
		Status:    queryStr(r, "status", ""),
		Limit:     queryInt(r, "limit", 50),
		Offset:    queryInt(r, "offset", 0),
	}
	events, err := eventing.ListEvents(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"), f)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "count": len(events)})
}

func (s *Server) GetTrace(w http.ResponseWriter, r *http.Request) {
	trace, err := player.GetTrace(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"), pathID(r, "eventId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, trace)
}

// ─────────────────────────────────────────────────────────────────────────────
// Player state (SDK-facing reads)
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) GetPlayerState(w http.ResponseWriter, r *http.Request) {
	st, err := player.GetFullState(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"), pathID(r, "userId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) GetLeaderboard(w http.ResponseWriter, r *http.Request) {
	page, err := player.GetLeaderboard(r.Context(), s.Pool,
		pathID(r, "projectId"), pathID(r, "environmentId"), pathID(r, "leaderboardId"),
		queryStr(r, "window", "ever"), queryStr(r, "me", ""), queryInt(r, "limit", 50))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// ─────────────────────────────────────────────────────────────────────────────
// Economy direct operations (§27)
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) AdjustWallet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Currency      string `json:"currency"`
		Amount        int64  `json:"amount"`
		Reference     string `json:"reference"`
		Reason        string `json:"reason"`
		AllowNegative bool   `json:"allow_negative"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Currency == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "validation_failed", "message": "currency: required"},
		})
		return
	}
	if req.Reference == "" {
		req.Reference = db.NewID("ref")
	}
	balance, applied, err := player.AdjustWallet(r.Context(), s.Pool,
		pathID(r, "projectId"), pathID(r, "environmentId"), pathID(r, "userId"),
		req.Currency, req.Amount, "api:"+actorOf(r), req.Reference, req.Reason, req.AllowNegative)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"balance":           balance,
		"applied":           applied,
		"currency":          req.Currency,
		"reference":         req.Reference,
		"idempotent_replay": !applied,
	})
}

func (s *Server) WalletHistory(w http.ResponseWriter, r *http.Request) {
	entries, err := player.WalletHistory(r.Context(), s.Pool,
		pathID(r, "projectId"), pathID(r, "environmentId"), pathID(r, "userId"),
		pathID(r, "currency"), queryInt(r, "limit", 50), queryInt(r, "offset", 0))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "count": len(entries)})
}

// Empty slice helper: Go nil slices serialize as null; API contracts prefer [].
func nonNilEvents(v []map[string]any) []map[string]any {
	if v == nil {
		return []map[string]any{}
	}
	return v
}

var _ = strings.TrimSpace
