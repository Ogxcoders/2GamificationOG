// MCP (Model Context Protocol, §58) JSON-RPC tools + simulator endpoints.
package api

import (
	"encoding/json"
	"net/http"

	"universalengagement/control-plane/internal/configsvc"
	"universalengagement/control-plane/internal/db"
	"universalengagement/control-plane/internal/player"
)

// jsonrpcRequest is the MCP JSON-RPC envelope.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  map[string]any  `json:"params,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// HandleMCP serves the JSON-RPC surface: tools/list, tools/call.
func (s *Server) HandleMCP(w http.ResponseWriter, r *http.Request) {
	var req jsonrpcRequest
	if !readJSON(w, r, &req) {
		return
	}
	projectID := pathID(r, "projectId")
	envID := pathID(r, "environmentId")

	switch req.Method {
	case "tools/list":
		writeJSON(w, http.StatusOK, mcpEnvelope(req.ID, map[string]any{
			"tools": mcpTools(),
		}, nil))
	case "tools/call":
		tool, _ := req.Params["name"].(string)
		args, _ := req.Params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		result, err := s.mcpCallTool(r, tool, args, projectID, envID)
		if err != nil {
			writeJSON(w, http.StatusOK, mcpEnvelope(req.ID, nil, &jsonrpcError{Code: -32000, Message: err.Error()}))
			return
		}
		writeJSON(w, http.StatusOK, mcpEnvelope(req.ID, map[string]any{"content": []map[string]any{
			{"type": "text", "text": result},
		}}, nil))
	default:
		writeJSON(w, http.StatusOK, mcpEnvelope(req.ID, nil, &jsonrpcError{Code: -32601, Message: "method not found: " + req.Method}))
	}
}

func mcpEnvelope(id json.RawMessage, result any, rpcErr *jsonrpcError) map[string]any {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	return resp
}

func mcpTools() []map[string]any {
	return []map[string]any{
		{
			"name":        "list_config_objects",
			"description": "List authored configuration objects (rules, challenges, streaks...) with status and version.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type": map[string]any{"type": "string", "description": "filter by object type (optional)"},
				},
			},
		},
		{
			"name":        "explain_event",
			"description": "Fetch the decision trace for one processed event: every rule evaluated, action executed, and command applied — the WHY behind an outcome.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"event_id": map[string]any{"type": "string"}},
				"required":   []string{"event_id"},
			},
		},
		{
			"name":        "player_state",
			"description": "Read the full gamification state of one user: XP tracks, wallets, challenges, streaks, achievements, entitlements.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"user_id": map[string]any{"type": "string"}},
				"required":   []string{"user_id"},
			},
		},
		{
			"name":        "compile_check",
			"description": "Validate the compiled engine configuration (duplicate ids, unknown currency references, action sanity).",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "simulate",
			"description": "Run a deterministic cohort simulation against the compiled config: seed, users, days.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"seed":  map[string]any{"type": "integer"},
					"users": map[string]any{"type": "integer"},
					"days":  map[string]any{"type": "integer"},
				},
			},
		},
	}
}

func (s *Server) mcpCallTool(r *http.Request, tool string, args map[string]any, projectID, envID string) (string, error) {
	switch tool {
	case "list_config_objects":
		objType, _ := args["type"].(string)
		objs, err := configsvc.List(r.Context(), s.Pool, projectID, envID, objType)
		if err != nil {
			return "", err
		}
		return marshalString(map[string]any{"objects": objs, "count": len(objs)}), nil
	case "explain_event":
		eventID, _ := args["event_id"].(string)
		if eventID == "" {
			return "", errMissingArg("event_id")
		}
		// Delegate to the player package via handler (same query).
		trace, err := s.traceFor(r, projectID, envID, eventID)
		if err != nil {
			return "", err
		}
		return marshalString(trace), nil
	case "player_state":
		userID, _ := args["user_id"].(string)
		if userID == "" {
			return "", errMissingArg("user_id")
		}
		st, err := s.playerStateFor(r, projectID, envID, userID)
		if err != nil {
			return "", err
		}
		return marshalString(st), nil
	case "compile_check":
		cfg, err := configsvc.CurrentEngineConfig(r.Context(), s.Pool, projectID, envID)
		if err != nil {
			return "", err
		}
		res, err := s.Eng.CompileCheck(r.Context(), cfg.Config)
		if err != nil {
			return "", err
		}
		return marshalString(res), nil
	case "simulate":
		seed := argInt(args, "seed", 42)
		users := argInt(args, "users", 100)
		days := argInt(args, "days", 7)
		cfg, err := configsvc.CurrentEngineConfig(r.Context(), s.Pool, projectID, envID)
		if err != nil {
			return "", err
		}
		res, err := s.Eng.Simulate(r.Context(), map[string]any{
			"seed": seed, "users": users, "days": days, "config": cfg.Config,
		})
		if err != nil {
			return "", err
		}
		return marshalString(res), nil
	default:
		return "", &missingArgError{"tool", "unknown tool '" + tool + "' — call tools/list"}
	}
}

type missingArgError struct{ field, msg string }

func (e *missingArgError) Error() string { return e.field + ": " + e.msg }

func errMissingArg(field string) error { return &missingArgError{field, "is required"} }

func argInt(args map[string]any, key string, def int) int {
	if v, ok := args[key].(float64); ok {
		return int(v)
	}
	return def
}

// ─────────────────────────────────────────────────────────────────────────────
// Simulator HTTP endpoint (§71)
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) Simulate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seed  int `json:"seed"`
		Users int `json:"users"`
		Days  int `json:"days"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Users <= 0 || req.Users > 10000 {
		req.Users = 100
	}
	if req.Days <= 0 || req.Days > 90 {
		req.Days = 7
	}
	cfg, err := configsvc.CurrentEngineConfig(r.Context(), s.Pool, pathID(r, "projectId"), pathID(r, "environmentId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.Eng.Simulate(r.Context(), map[string]any{
		"seed": req.Seed, "users": req.Users, "days": req.Days, "config": cfg.Config,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// Persist the run record for audit.
	_, _ = s.Pool.Exec(r.Context(), `
                INSERT INTO simulation_runs (id, project_id, environment_id, seed, users, days, config_version, report)
                VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		newSimID(), pathID(r, "projectId"), pathID(r, "environmentId"), req.Seed, req.Users, req.Days, int64(cfg.Version), res)
	writeJSON(w, http.StatusOK, res)
}

func newSimID() string { return db.NewID("sim") }

// ── MCP helper plumbing ───────────────────────────────────────────────────────

func marshalString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"marshal failed"}`
	}
	return string(b)
}

func (s *Server) traceFor(r *http.Request, projectID, envID, eventID string) (map[string]any, error) {
	return player.GetTrace(r.Context(), s.Pool, projectID, envID, eventID)
}

func (s *Server) playerStateFor(r *http.Request, projectID, envID, userID string) (any, error) {
	return player.GetFullState(r.Context(), s.Pool, projectID, envID, userID)
}
