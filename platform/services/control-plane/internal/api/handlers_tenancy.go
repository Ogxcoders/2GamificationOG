// Tenancy + identity handlers: bootstrap, projects, environments, API keys, users.
package api

import (
	"encoding/json"
	"net/http"

	"universalengagement/control-plane/internal/identity"
	"universalengagement/control-plane/internal/tenancy"
)

// Bootstrap provisions org + project + 3 environments + the initial admin key
// in one call (Customer Zero golden-path step 1).
func (s *Server) Bootstrap(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Organization string `json:"organization"`
		Project      string `json:"project"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Organization == "" || req.Project == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "validation_failed", "message": "organization and project names are required"},
		})
		return
	}
	org, project, envs, key, err := tenancy.BootstrapTenant(r.Context(), s.Pool, req.Organization, req.Project)
	if err != nil {
		writeErr(w, err)
		return
	}
	envList := []map[string]any{}
	for _, e := range envs {
		envList = append(envList, map[string]any{"id": e.ID, "kind": e.Kind})
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"organization": map[string]any{"id": org.ID, "name": org.Name, "slug": org.Slug},
		"project":      map[string]any{"id": project.ID, "name": project.Name, "timezone": project.Timezone},
		"environments": envList,
		"api_key":      key.CreatedKey(),
		"key_scopes":   key.Scopes,
		"note":         "store the API key now — the secret part is never shown again",
	})
}

func (s *Server) ListProjects(w http.ResponseWriter, r *http.Request) {
	orgID := r.URL.Query().Get("organization_id")
	if orgID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "validation_failed", "message": "organization_id query parameter is required"},
		})
		return
	}
	projects, err := tenancy.ListProjects(r.Context(), s.Pool, orgID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects, "count": len(projects)})
}

func (s *Server) GetProject(w http.ResponseWriter, r *http.Request) {
	project, err := tenancy.GetProject(r.Context(), s.Pool, pathID(r, "projectId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) ListEnvironments(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), `
                SELECT jsonb_build_object('id', id, 'kind', kind, 'name', name)
                FROM environments WHERE project_id=$1 ORDER BY kind`,
		pathID(r, "projectId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			writeErr(w, err)
			return
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"environments": out, "count": len(out)})
}

func (s *Server) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ProjectID string   `json:"project_id"`
		EnvID     string   `json:"environment_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "validation_failed", "message": "name is required"},
		})
		return
	}
	projectID := pathID(r, "projectId")
	if projectID == "" {
		projectID = req.ProjectID
	}
	envID := pathID(r, "environmentId")
	if envID == "" {
		envID = req.EnvID
	}
	if len(req.Scopes) == 0 {
		req.Scopes = []string{"events:write", "state:read"}
	}
	key, err := tenancy.CreateAPIKey(r.Context(), s.Pool, projectID, envID, req.Name, req.Scopes)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"key_id":  key.ID,
		"api_key": key.CreatedKey(),
		"scopes":  key.Scopes,
		"note":    "store the secret now — it is never shown again",
	})
}

func (s *Server) RevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := tenancy.RevokeAPIKey(r.Context(), s.Pool, pathID(r, "keyId")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "revoked"})
}

// ─────────────────────────────────────────────────────────────────────────────
// Users (§9)
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) UpsertUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID      string         `json:"user_id"`
		Anonymous   bool           `json:"anonymous"`
		DisplayName string         `json:"display_name"`
		Attributes  map[string]any `json:"attributes"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.UserID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "validation_failed", "message": "user_id is required"},
		})
		return
	}
	user, err := identity.UpsertUser(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"),
		req.UserID, req.Anonymous, req.DisplayName, req.Attributes)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) GetUser(w http.ResponseWriter, r *http.Request) {
	user, err := identity.GetUser(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"), pathID(r, "userId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) LinkIdentity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider       string `json:"provider"`
		ProviderUserID string `json:"provider_user_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Provider == "" || req.ProviderUserID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "validation_failed", "message": "provider and provider_user_id are required"},
		})
		return
	}
	if err := identity.LinkIdentity(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"),
		pathID(r, "userId"), req.Provider, req.ProviderUserID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) MergeAnonymous(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceUserID string `json:"source_user_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.SourceUserID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "validation_failed", "message": "source_user_id is required (the anonymous account to merge INTO this user)"},
		})
		return
	}
	if err := identity.MergeAnonymous(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"),
		req.SourceUserID, pathID(r, "userId")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "merged": req.SourceUserID, "into": pathID(r, "userId")})
}
