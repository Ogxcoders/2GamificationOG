// Monetization (§37-§45), notifications, webhooks CRUD, transactions.
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"universalengagement/control-plane/internal/db"
)

// ─────────────────────────────────────────────────────────────────────────────
// Products / offers / paywalls ride the generic config-object lifecycle
// (CreateObject etc. in handlers.go) — these handlers cover the runtime side.
// ─────────────────────────────────────────────────────────────────────────────

// Purchase (mock provider flow §41): idempotent transaction + entitlement grant.
func (s *Server) Purchase(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID    string `json:"user_id"`
		ProductID string `json:"product_id"`
		OfferID   string `json:"offer_id"`
		IdemKey   string `json:"idempotency_key"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.UserID == "" || req.ProductID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "validation_failed", "message": "user_id and product_id are required"},
		})
		return
	}
	projectID := pathID(r, "projectId")
	envID := pathID(r, "environmentId")

	// Resolve product price + kind.
	var priceCents int
	var kind, name string
	err := s.Pool.QueryRow(r.Context(), `
		SELECT COALESCE(price_cents,0), COALESCE(kind,'consumable'), COALESCE(name,'') FROM products
		WHERE id=$1 AND project_id=$2 AND environment_id=$3`, req.ProductID, projectID, envID).
		Scan(&priceCents, &kind, &name)
	if err != nil {
		writeErr(w, db.NotFoundError("product not found in this environment"))
		return
	}

	// Offer discount (optional).
	if req.OfferID != "" {
		var discount int
		if err := s.Pool.QueryRow(r.Context(), `
			SELECT COALESCE(discount_percent,0) FROM offers
			WHERE id=$1 AND project_id=$2 AND environment_id=$3`, req.OfferID, projectID, envID).
			Scan(&discount); err == nil && discount > 0 {
			priceCents = priceCents * (100 - discount) / 100
		}
	}

	txID := db.NewID("txn")
	if req.IdemKey == "" {
		req.IdemKey = txID
	}

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	// Idempotent transaction insert.
	tag, err := tx.Exec(r.Context(), `
		INSERT INTO transactions (id, project_id, environment_id, user_id, product_id, offer_id, amount_cents, provider, status, idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'mock','completed',$8)
		ON CONFLICT (project_id, environment_id, idempotency_key) DO NOTHING`,
		txID, projectID, envID, req.UserID, req.ProductID, req.OfferID, priceCents, req.IdemKey)
	if err != nil {
		writeErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "duplicate", "message": "this idempotency key was already used — the original transaction stands",
			"idempotency_key": req.IdemKey,
		})
		return
	}

	// Grant effects by product kind.
	switch kind {
	case "subscription":
		_, err = tx.Exec(r.Context(), `
			INSERT INTO subscriptions (id, project_id, environment_id, user_id, product_id, status, current_period_start, current_period_end)
			VALUES ($1,$2,$3,$4,$5,'active',now(), now() + interval '30 days')`,
			db.NewID("sub"), projectID, envID, req.UserID, req.ProductID)
	case "entitlement":
		_, err = tx.Exec(r.Context(), `
			INSERT INTO entitlements (id, project_id, environment_id, user_id, entitlement, source)
			VALUES ($1,$2,$3,$4,$5,'purchase')`,
			db.NewID("ent"), projectID, envID, req.UserID, "product:"+req.ProductID)
	case "credits":
		// Credits map to a currency ledger grant (currency = product name slug).
		_, err = tx.Exec(r.Context(), `
			INSERT INTO ledger_entries (entry_id, project_id, environment_id, wallet_user_id, currency, amount, source, reason, reference)
			VALUES ($1,$2,$3,$4,$5,$6,'purchase',$7,$8)
			ON CONFLICT (project_id, environment_id, source, reference) DO NOTHING`,
			db.NewID("led"), projectID, envID, req.UserID, db.Slugify(name), priceCents, "purchase of "+name, txID)
		if err == nil {
			_, err = tx.Exec(r.Context(), `
				INSERT INTO wallets (project_id, environment_id, user_id, currency, balance)
				VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (project_id, environment_id, user_id, currency) DO UPDATE SET balance = wallets.balance + $5`,
				projectID, envID, req.UserID, db.Slugify(name), priceCents)
		}
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"transaction_id": txID, "status": "completed", "product": name,
		"amount_cents": priceCents, "kind": kind,
	})
}

// ListTransactions pages a user's purchase history.
func (s *Server) ListTransactions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), `
		SELECT jsonb_build_object('id', t.id, 'product_id', t.product_id, 'amount_cents', t.amount_cents,
		       'status', t.status, 'provider', t.provider,
		       'created_at', to_char(t.created_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		FROM transactions t
		WHERE t.project_id=$1 AND t.environment_id=$2 AND t.user_id=$3
		ORDER BY t.created_at DESC LIMIT $4 OFFSET $5`,
		pathID(r, "projectId"), pathID(r, "environmentId"), pathID(r, "userId"),
		queryInt(r, "limit", 50), queryInt(r, "offset", 0))
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			writeErr(w, err)
			return
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"transactions": out, "count": len(out)})
}

// ─────────────────────────────────────────────────────────────────────────────
// Webhook endpoints (§99) — HMAC-signed delivery
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) CreateWebhook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL         string   `json:"url"`
		Events      []string `json:"events"`
		Description string   `json:"description"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.URL == "" || !strings_HasPrefixHTTP(req.URL) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "validation_failed", "message": "url: must be an http(s) URL"},
		})
		return
	}
	secret := db.RandomToken(32)
	id := db.NewID("wh")
	_, err := s.Pool.Exec(r.Context(), `
		INSERT INTO webhook_endpoints (id, project_id, environment_id, url, events, secret, description)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, pathID(r, "projectId"), pathID(r, "environmentId"), req.URL, req.Events, secret, req.Description)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "url": req.URL, "events": req.Events,
		"secret": secret,
		"note":   "store this secret now — it signs deliveries (X-Signature: hex(hmac-sha256)) and is never shown again",
	})
}

func strings_HasPrefixHTTP(u string) bool {
	return len(u) > 7 && (u[:7] == "http://" || u[:8] == "https://")
}

func (s *Server) ListWebhooks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), `
		SELECT jsonb_build_object('id', id, 'url', url, 'events', events, 'description', description,
		       'active', active, 'created_at', to_char(created_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		FROM webhook_endpoints WHERE project_id=$1 AND environment_id=$2 ORDER BY created_at DESC`,
		pathID(r, "projectId"), pathID(r, "environmentId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			writeErr(w, err)
			return
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": out, "count": len(out)})
}

func (s *Server) DeleteWebhook(w http.ResponseWriter, r *http.Request) {
	tag, err := s.Pool.Exec(r.Context(), `
		DELETE FROM webhook_endpoints WHERE id=$1 AND project_id=$2 AND environment_id=$3`,
		pathID(r, "webhookId"), pathID(r, "projectId"), pathID(r, "environmentId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, db.NotFoundError("webhook endpoint not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// SignWebhook computes the delivery signature (used by workers + tests).
func SignWebhook(secret string, body []byte, timestamp int64) string {
	payload := append([]byte(time.Unix(timestamp, 0).UTC().Format(time.RFC3339)), '.')
	payload = append(payload, body...)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// ─────────────────────────────────────────────────────────────────────────────
// Notifications (§75)
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) ListNotifications(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), `
		SELECT jsonb_build_object('id', id, 'template', template, 'params', params, 'channel', channel,
		       'status', status, 'created_at', to_char(created_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		FROM notifications WHERE project_id=$1 AND environment_id=$2 AND user_id=$3
		ORDER BY created_at DESC LIMIT $4 OFFSET $5`,
		pathID(r, "projectId"), pathID(r, "environmentId"), pathID(r, "userId"),
		queryInt(r, "limit", 50), queryInt(r, "offset", 0))
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			writeErr(w, err)
			return
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": out, "count": len(out)})
}

// ─────────────────────────────────────────────────────────────────────────────
// Segments + flags evaluation (§33-§36)
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) EvaluateFlag(w http.ResponseWriter, r *http.Request) {
	key := pathID(r, "flagKey")
	userID := queryStr(r, "user_id", "")
	var value json.RawMessage
	var rollout int
	var overrides json.RawMessage
	err := s.Pool.QueryRow(r.Context(), `
		SELECT value, rollout_percent, user_overrides FROM feature_flags
		WHERE project_id=$1 AND environment_id=$2 AND key=$3`,
		pathID(r, "projectId"), pathID(r, "environmentId"), key).
		Scan(&value, &rollout, &overrides)
	if err != nil {
		writeErr(w, db.NotFoundError("flag '"+key+"' not found in this environment"))
		return
	}
	// User overrides beat rollouts; rollouts bucket deterministically by hash.
	if userID != "" {
		var ov map[string]json.RawMessage
		_ = json.Unmarshal(overrides, &ov)
		if v, ok := ov[userID]; ok {
			writeJSON(w, http.StatusOK, map[string]any{"key": key, "value": json.RawMessage(v), "source": "override"})
			return
		}
	}
	bucket := hashBucket(pathID(r, "projectId")+":"+key+":"+userID) % 100
	enabled := bucket < rollout
	var out any = enabled
	if !enabled {
		out = json.RawMessage("false")
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "value": out, "source": "rollout", "rollout_percent": rollout})
}

func hashBucket(s string) int {
	sum := sha256.Sum256([]byte(s))
	return int(sum[0])<<8 | int(sum[1])
}

func (s *Server) ListSegments(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), `
		SELECT jsonb_build_object('id', id, 'name', name, 'definition', definition,
		       'created_at', to_char(created_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		FROM segments WHERE project_id=$1 AND environment_id=$2 ORDER BY name`,
		pathID(r, "projectId"), pathID(r, "environmentId"))
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			writeErr(w, err)
			return
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"segments": out, "count": len(out)})
}

// ─────────────────────────────────────────────────────────────────────────────
// Analytics rollups (§76)
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) AnalyticsOverview(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 7)
	if days < 1 || days > 90 {
		days = 7
	}
	out := map[string]any{}

	// Events by day.
	rows, err := s.Pool.Query(r.Context(), `
		SELECT to_char(date_trunc('day', occurred_at), 'YYYY-MM-DD') AS day, COUNT(*)
		FROM events
		WHERE project_id=$1 AND environment_id=$2 AND occurred_at > now() - ($3 || ' days')::interval
		GROUP BY 1 ORDER BY 1`,
		pathID(r, "projectId"), pathID(r, "environmentId"), days)
	if err == nil {
		defer rows.Close()
		byDay := []map[string]any{}
		for rows.Next() {
			var day string
			var count int64
			_ = rows.Scan(&day, &count)
			byDay = append(byDay, map[string]any{"day": day, "events": count})
		}
		out["events_by_day"] = byDay
	}

	// Top rules fired (from traces' matched rules).
	rows2, err := s.Pool.Query(r.Context(), `
		SELECT jsonb_array_elements(nodes)->>'label' AS label, COUNT(*)
		FROM decision_traces
		WHERE project_id=$1 AND environment_id=$2 AND created_at > now() - interval '7 days'
		  AND jsonb_array_elements(nodes)->>'kind' = 'rule_matched'
		GROUP BY 1 ORDER BY 2 DESC LIMIT 10`,
		pathID(r, "projectId"), pathID(r, "environmentId"))
	if err == nil {
		defer rows2.Close()
		topRules := []map[string]any{}
		for rows2.Next() {
			var label string
			var count int64
			_ = rows2.Scan(&label, &count)
			topRules = append(topRules, map[string]any{"label": label, "fires": count})
		}
		out["top_rules"] = topRules
	}

	// DAU (distinct actors per day).
	rows3, err := s.Pool.Query(r.Context(), `
		SELECT to_char(date_trunc('day', occurred_at), 'YYYY-MM-DD') AS day, COUNT(DISTINCT actor_id)
		FROM events
		WHERE project_id=$1 AND environment_id=$2 AND occurred_at > now() - ($3 || ' days')::interval
		GROUP BY 1 ORDER BY 1`,
		pathID(r, "projectId"), pathID(r, "environmentId"), days)
	if err == nil {
		defer rows3.Close()
		dau := []map[string]any{}
		for rows3.Next() {
			var day string
			var users int64
			_ = rows3.Scan(&day, &users)
			dau = append(dau, map[string]any{"day": day, "users": users})
		}
		out["dau"] = dau
	}

	writeJSON(w, http.StatusOK, out)
}

// ─────────────────────────────────────────────────────────────────────────────
// Audit (§96)
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) ListAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), `
		SELECT jsonb_build_object('id', a.id, 'action', a.action, 'actor_type', a.actor_type,
		       'actor_id', a.actor_id, 'subject_type', a.subject_type, 'subject_id', a.subject_id,
		       'reason', a.reason, 'occurred_at', to_char(a.occurred_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		FROM audit_log a
		WHERE a.project_id=$1 AND ($2='' OR a.environment_id=$2)
		ORDER BY a.occurred_at DESC LIMIT $3 OFFSET $4`,
		pathID(r, "projectId"), queryStr(r, "environment", ""), queryInt(r, "limit", 50), queryInt(r, "offset", 0))
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			writeErr(w, err)
			return
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "count": len(out)})
}
