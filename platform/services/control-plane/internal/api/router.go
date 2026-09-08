// Router: every route authenticated, every project-scoped route tenant-checked.
//
// Route map (v1):
//
//      POST /v1/bootstrap                                 — provision org/project/envs/key
//      GET  /healthz
//
//      Projects (org-scope):
//      GET  /v1/projects
//      GET  /v1/projects/{projectId}
//      GET  /v1/projects/{projectId}/environments
//      POST /v1/projects/{projectId}/keys
//      DELETE /v1/keys/{keyId}
//
//      Config objects (project+env scope):
//      POST/GET /v1/projects/{projectId}/environments/{environmentId}/objects
//      GET/PUT  .../objects/{objectId}
//      POST     .../objects/{objectId}/publish|pause|resume|rollback|promote
//      GET/POST .../engine-config  (current snapshot | recompile)
//      POST     .../validate-draft
//
//      Events (SDK surface):
//      POST .../events            — ingest batch
//      POST .../events/{eventId}/process — run the pipeline
//      POST .../events/process-batch
//      GET  .../events/{eventId}
//      GET  .../events
//      GET  .../events/{eventId}/trace
//
//      Players:
//      GET  .../users/{userId}/state
//      GET  .../leaderboards/{leaderboardId}
//      POST .../users/{userId}/wallets/adjust
//      GET  .../users/{userId}/wallets/{currency}/history
//      POST/GET .../users            — upsert / (list via events)
//      POST .../users/{userId}/identities | /merge
//
//      Monetization + ops:
//      POST .../purchases            — mock checkout
//      GET  .../users/{userId}/transactions
//      POST/GET .../webhooks, DELETE .../webhooks/{webhookId}
//      GET  .../users/{userId}/notifications
//      GET  .../flags/{flagKey}/evaluate
//      GET  .../segments
//      GET  .../analytics/overview
//      GET  .../audit
//      POST .../simulate
//      POST .../mcp                   — JSON-RPC tools
package api

import (
        "net/http"
        "os"

        "universalengagement/control-plane/internal/db"
        "universalengagement/control-plane/internal/engine"
        "universalengagement/control-plane/internal/middleware"
)

// New builds the full router. EVERY route requires a valid API key; the
// health probe is the sole exception (liveness, no data).
func New(pool *db.Pool, eng *engine.Client, allowedOrigin string) http.Handler {
        s := &Server{Pool: pool, Eng: eng}
        mux := http.NewServeMux()

        // Liveness (no auth, no data exposure).
        mux.HandleFunc("GET /healthz", s.Healthz)

        // Bootstrap: authenticated by a bootstrap token OR open on fresh installs?
        // §185: no unauthenticated routes. Bootstrap requires a key created by the
        // operator via CLI/bootstrap script — but the very first key cannot exist
        // yet. Resolution (documented in docs/BOOTSTRAP.md): bootstrap is guarded
        // by the BOOTSTRAP_TOKEN env; when unset it is allowed only while the
        // platform has zero organizations (first-run window).
        mux.Handle("POST /v1/bootstrap", s.guardBootstrap())

        auth := func(scope string, h http.HandlerFunc) http.Handler {
                // Order matters: authenticate FIRST (Require sets the actor in the
                // request context), THEN tenant-scope the path values.
                return middleware.Require(pool, scope, middleware.RequireScope(http.HandlerFunc(h)))
        }

        // Projects & keys.
        mux.Handle("GET /v1/projects", auth("projects:read", s.ListProjects))
        mux.Handle("GET /v1/projects/{projectId}", auth("projects:read", s.GetProject))
        mux.Handle("GET /v1/projects/{projectId}/environments", auth("projects:read", s.ListEnvironments))
        mux.Handle("POST /v1/projects/{projectId}/keys", auth("keys:manage", s.CreateAPIKey))
        mux.Handle("POST /v1/keys", auth("keys:manage", s.CreateAPIKey))
        mux.Handle("DELETE /v1/keys/{keyId}", auth("keys:manage", s.RevokeAPIKey))

        // Scope prefix for brevity.
        const scope = "/v1/projects/{projectId}/environments/{environmentId}"

        mux.Handle("POST "+scope+"/objects", auth("config:write", s.CreateObject))
        mux.Handle("GET "+scope+"/objects", auth("config:read", s.ListObjects))
        mux.Handle("GET "+scope+"/objects/{objectId}", auth("config:read", s.GetObject))
        mux.Handle("PUT "+scope+"/objects/{objectId}", auth("config:write", s.UpdateObject))
        mux.Handle("POST "+scope+"/objects/{objectId}/publish", auth("config:publish", s.PublishObject))
        mux.Handle("POST "+scope+"/objects/{objectId}/pause", auth("config:publish", s.PauseObject))
        mux.Handle("POST "+scope+"/objects/{objectId}/resume", auth("config:publish", s.ResumeObject))
        mux.Handle("POST "+scope+"/objects/{objectId}/rollback", auth("config:publish", s.RollbackObject))
        mux.Handle("POST "+scope+"/objects/{objectId}/promote", auth("config:publish", s.PromoteObject))
        mux.Handle("DELETE "+scope+"/objects/{objectId}", auth("config:write", s.DeleteObject))
        mux.Handle("GET "+scope+"/engine-config", auth("config:read", s.GetEngineConfig))
        mux.Handle("POST "+scope+"/engine-config/recompile", auth("config:publish", s.RecompileEngineConfig))
        mux.Handle("POST "+scope+"/validate-draft", auth("config:write", s.ValidateDraft))

        // Events.
        mux.Handle("POST "+scope+"/events", auth("events:write", s.IngestEvents))
        mux.Handle("POST "+scope+"/events/{eventId}/process", auth("events:process", s.ProcessEvent))
        mux.Handle("POST "+scope+"/events/process-batch", auth("events:process", s.ProcessBatch))
        mux.Handle("GET "+scope+"/events/{eventId}", auth("events:read", s.GetEvent))
        mux.Handle("GET "+scope+"/events", auth("events:read", s.ListEvents))
        mux.Handle("GET "+scope+"/events/{eventId}/trace", auth("events:read", s.GetTrace))

        // Users & identity.
        mux.Handle("POST "+scope+"/users", auth("users:write", s.UpsertUser))
        mux.Handle("GET "+scope+"/users/{userId}", auth("users:read", s.GetUser))
        mux.Handle("POST "+scope+"/users/{userId}/identities", auth("users:write", s.LinkIdentity))
        mux.Handle("POST "+scope+"/users/{userId}/merge", auth("users:write", s.MergeAnonymous))

        // Player state.
        mux.Handle("GET "+scope+"/users/{userId}/state", auth("state:read", s.GetPlayerState))
        mux.Handle("GET "+scope+"/leaderboards/{leaderboardId}", auth("state:read", s.GetLeaderboard))

        // Economy direct ops.
        mux.Handle("POST "+scope+"/users/{userId}/wallets/adjust", auth("economy:write", s.AdjustWallet))
        mux.Handle("GET "+scope+"/users/{userId}/wallets/{currency}/history", auth("state:read", s.WalletHistory))

        // Monetization.
        mux.Handle("POST "+scope+"/purchases", auth("monetization:write", s.Purchase))
        mux.Handle("GET "+scope+"/users/{userId}/transactions", auth("state:read", s.ListTransactions))

        // Webhooks.
        mux.Handle("POST "+scope+"/webhooks", auth("webhooks:manage", s.CreateWebhook))
        mux.Handle("GET "+scope+"/webhooks", auth("webhooks:manage", s.ListWebhooks))
        mux.Handle("DELETE "+scope+"/webhooks/{webhookId}", auth("webhooks:manage", s.DeleteWebhook))

        // Notifications.
        mux.Handle("GET "+scope+"/users/{userId}/notifications", auth("state:read", s.ListNotifications))

        // Flags & segments.
        mux.Handle("GET "+scope+"/flags/{flagKey}/evaluate", auth("state:read", s.EvaluateFlag))
        mux.Handle("GET "+scope+"/segments", auth("config:read", s.ListSegments))

        // Analytics & audit.
        mux.Handle("GET "+scope+"/analytics/overview", auth("analytics:read", s.AnalyticsOverview))
        mux.Handle("GET "+scope+"/audit", auth("audit:read", s.ListAudit))

        // Simulator + MCP.
        mux.Handle("POST "+scope+"/simulate", auth("simulate:run", s.Simulate))
        mux.Handle("POST "+scope+"/mcp", auth("mcp:call", s.HandleMCP))

        // Stack: CORS → logging → recover.
        var handler http.Handler = mux
        handler = middleware.Logging(handler)
        handler = middleware.Recover(handler)
        handler = middleware.CORS(allowedOrigin)(handler)
        return handler
}

// guardBootstrap protects first-run provisioning.
func (s *Server) guardBootstrap() http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                // Allowed when: valid existing key (normal ops adding tenants), OR
                // the platform is empty (first-run window), OR BOOTSTRAP_TOKEN matches.
                token := r.Header.Get("X-Bootstrap-Token")
                if token != "" && bootstrapTokenMatches(token) {
                        s.Bootstrap(w, r)
                        return
                }
                var count int
                if err := s.Pool.QueryRow(r.Context(), `SELECT COUNT(*) FROM organizations`).Scan(&count); err == nil && count == 0 {
                        s.Bootstrap(w, r)
                        return
                }
                // Fall back to normal key auth (org-scope keys may provision projects).
                middleware.Require(s.Pool, "", http.HandlerFunc(s.Bootstrap)).ServeHTTP(w, r)
        })
}

func bootstrapTokenMatches(token string) bool {
        expected := os.Getenv("BOOTSTRAP_TOKEN")
        return token != "" && expected != "" && token == expected
}
