// Package api: HTTP handlers for the control plane. Every route is
// authenticated (no exceptions — the unauthenticated-route lesson) and every
// project-scoped route enforces tenant isolation.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"universalengagement/control-plane/internal/configsvc"
	"universalengagement/control-plane/internal/db"
	"universalengagement/control-plane/internal/engine"
)

// Server carries handler dependencies.
type Server struct {
	Pool *db.Pool
	Eng  *engine.Client
}

// EngineClient exposes the engine client to handlers.
func (s *Server) EngineClient() *engine.Client { return s.Eng }

// ── response helpers ──────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ue *db.UserError
	if errors.As(err, &ue) {
		writeJSON(w, ue.Status, map[string]any{
			"error": map[string]any{"code": ue.Code, "message": ue.Message},
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"error": map[string]any{"code": "internal_error", "message": err.Error()},
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"code": "read_body_failed", "message": err.Error()},
		})
		return false
	}
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"code": "empty_body", "message": "a JSON body is required"},
		})
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"code": "invalid_json", "message": "body is not valid JSON: " + err.Error()}},
		)
		return false
	}
	return true
}

func pathID(r *http.Request, name string) string { return r.PathValue(name) }

func queryInt(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

func queryStr(r *http.Request, key, def string) string {
	if v := r.URL.Query().Get(key); v != "" {
		return v
	}
	return def
}

// ── health ────────────────────────────────────────────────────────────────────

func (s *Server) Healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "control-plane"})
}

// ─────────────────────────────────────────────────────────────────────────────
// Config object lifecycle (§6/§7)
// ─────────────────────────────────────────────────────────────────────────────

type objectRequest struct {
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Config map[string]any `json:"config"`
}

func (s *Server) CreateObject(w http.ResponseWriter, r *http.Request) {
	var req objectRequest
	if !readJSON(w, r, &req) {
		return
	}
	actor := actorOf(r)
	obj, err := configsvc.Create(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"),
		req.Type, req.Name, req.Config, actor)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, obj)
}

func (s *Server) GetObject(w http.ResponseWriter, r *http.Request) {
	obj, err := configsvc.Get(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"), pathID(r, "objectId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func (s *Server) ListObjects(w http.ResponseWriter, r *http.Request) {
	objs, err := configsvc.List(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"), queryStr(r, "type", ""))
	if err != nil {
		writeErr(w, err)
		return
	}
	if objs == nil {
		objs = []*configsvc.Object{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"objects": objs, "count": len(objs)})
}

func (s *Server) UpdateObject(w http.ResponseWriter, r *http.Request) {
	var req objectRequest
	if !readJSON(w, r, &req) {
		return
	}
	obj, err := configsvc.Update(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"),
		pathID(r, "objectId"), req.Config)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func (s *Server) PublishObject(w http.ResponseWriter, r *http.Request) {
	obj, err := configsvc.Publish(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"),
		pathID(r, "objectId"), actorOf(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func (s *Server) PauseObject(w http.ResponseWriter, r *http.Request) {
	if err := configsvc.Pause(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"),
		pathID(r, "objectId"), true); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "paused"})
}

func (s *Server) ResumeObject(w http.ResponseWriter, r *http.Request) {
	if err := configsvc.Pause(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"),
		pathID(r, "objectId"), false); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "active"})
}

func (s *Server) RollbackObject(w http.ResponseWriter, r *http.Request) {
	version := queryInt(r, "to_version", 0)
	obj, err := configsvc.Rollback(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"),
		pathID(r, "objectId"), version)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func (s *Server) DeleteObject(w http.ResponseWriter, r *http.Request) {
	if err := configsvc.Delete(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"),
		pathID(r, "objectId")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "archived"})
}

func (s *Server) PromoteObject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ToEnvironment string `json:"to_environment"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.ToEnvironment == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "validation_failed", "message": "to_environment is required (development|staging|production or an environment id)"},
		})
		return
	}
	obj, err := configsvc.Promote(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"),
		pathID(r, "objectId"), req.ToEnvironment, actorOf(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func (s *Server) GetEngineConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := configsvc.CurrentEngineConfig(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) RecompileEngineConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := configsvc.CompileEngineConfig(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) ValidateDraft(w http.ResponseWriter, r *http.Request) {
	var req objectRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := configsvc.ValidateObject(req.Type, req.Config); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}

func actorOf(r *http.Request) string {
	// API key id is the acting principal; audit trail records it.
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		parts := strings.SplitN(strings.TrimPrefix(auth, "Bearer "), ".", 2)
		if len(parts) == 1 && parts[0] != "" {
			return parts[0]
		}
		if len(parts) == 2 {
			return parts[0]
		}
	}
	return "api"
}
