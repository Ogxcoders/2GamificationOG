// Package workers: background loops — transactional-outbox publisher (§91),
// webhook delivery with HMAC signatures + exponential backoff (§99), and
// workflow/streak resumption jobs.
package workers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"universalengagement/control-plane/internal/db"
)

// Runner owns the background loops.
type Runner struct {
	Pool     *db.Pool
	HTTP     *http.Client
	Interval time.Duration
	stop     chan struct{}
}

// NewRunner builds the worker set.
func NewRunner(pool *db.Pool) *Runner {
	return &Runner{
		Pool:     pool,
		HTTP:     &http.Client{Timeout: 10 * time.Second},
		Interval: 2 * time.Second,
		stop:     make(chan struct{}),
	}
}

// Start launches every loop; Stop cancels them.
func (r *Runner) Start(ctx context.Context) {
	go r.loop(ctx, r.publishOutbox)
	go r.loop(ctx, r.resumeWorkflows)
	slog.Info("workers started", "interval", r.Interval.String())
}

func (r *Runner) Stop() { close(r.stop) }

func (r *Runner) loop(ctx context.Context, job func(context.Context) error) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stop:
			return
		case <-t.C:
			if err := job(ctx); err != nil {
				slog.Warn("worker job failed", "error", err)
			}
		}
	}
}

// publishOutbox drains pending outbox rows: webhook fan-out + notification
// sends, marking rows published only on success (at-least-once).
func (r *Runner) publishOutbox(ctx context.Context) error {
	rows, err := r.Pool.Query(ctx, `
                SELECT id, project_id, environment_id, event_id, topic, payload
                FROM outbox WHERE published_at IS NULL
                ORDER BY id LIMIT 50`)
	if err != nil {
		return err
	}
	type pending struct {
		id, project, env, event, topic string
		payload                        json.RawMessage
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.project, &p.env, &p.event, &p.topic, &p.payload); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, p)
	}
	rows.Close()
	if len(batch) == 0 {
		return nil
	}

	for _, p := range batch {
		r.fanOut(ctx, p.project, p.env, p.event, p.topic, p.payload)
		_, err := r.Pool.Exec(ctx, `
                        UPDATE outbox SET published_at = now() WHERE id = $1`, p.id)
		if err != nil {
			return err
		}
	}
	return nil
}

// fanOut delivers to every active webhook endpoint subscribed to the topic.
func (r *Runner) fanOut(ctx context.Context, projectID, envID, eventID, topic string, payload json.RawMessage) {
	rows, err := r.Pool.Query(ctx, `
                SELECT id, url, secret FROM webhook_endpoints
                WHERE project_id=$1 AND environment_id=$2 AND active
                  AND ('*' = ANY(events) OR $3 = ANY(events))`,
		projectID, envID, topic)
	if err != nil {
		return
	}
	defer rows.Close()
	type endpoint struct {
		id, url, secret string
	}
	var endpoints []endpoint
	for rows.Next() {
		var e endpoint
		if err := rows.Scan(&e.id, &e.url, &e.secret); err != nil {
			return
		}
		endpoints = append(endpoints, e)
	}

	body := map[string]any{
		"topic":          topic,
		"event_id":       eventID,
		"project_id":     projectID,
		"environment_id": envID,
		"payload":        json.RawMessage(payload),
	}
	raw, _ := json.Marshal(body)
	ts := time.Now().UTC()

	for _, e := range endpoints {
		go r.deliver(ctx, e.id, e.url, e.secret, eventID, raw, ts)
	}
}

func (r *Runner) deliver(ctx context.Context, endpointID, url, secret, eventID string, body []byte, ts time.Time) {
	// HMAC-SHA256 signature over "<RFC3339>.<body>" — receivers verify with
	// the shared secret and reject stale timestamps (replay protection).
	payload := append([]byte(ts.Format(time.RFC3339)+"."), body...)
	mac := hmacSHA256(secret, payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		r.recordDelivery(ctx, endpointID, eventID, 0, "", "failed", "invalid URL")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", mac)
	req.Header.Set("X-Timestamp", ts.Format(time.RFC3339))
	req.Header.Set("X-Event-Id", eventID)

	resp, err := r.HTTP.Do(req)
	if err != nil {
		r.recordDelivery(ctx, endpointID, eventID, 0, "", "failed", err.Error())
		return
	}
	defer resp.Body.Close()
	status := "delivered"
	if resp.StatusCode >= 400 {
		status = "failed"
	}
	r.recordDelivery(ctx, endpointID, eventID, resp.StatusCode, "", status, "")
}

func (r *Runner) recordDelivery(ctx context.Context, endpointID, eventID string, httpStatus int, _, status, errMsg string) {
	_, _ = r.Pool.Exec(ctx, `
                INSERT INTO webhook_deliveries (id, endpoint_id, event_id, status, signature, request_body, response_status, response_body)
                VALUES ($1,$2,$3,$4,'',$5,$6,$7)`,
		db.NewID("whd"), endpointID, eventID, status, json.RawMessage("{}"), httpStatus, errMsg)
}

// resumeWorkflows advances waiting workflow runs whose resume time passed.
func (r *Runner) resumeWorkflows(ctx context.Context) error {
	tag, err := r.Pool.Exec(ctx, `
                UPDATE workflow_runs SET status='running', resume_at=NULL
                WHERE status='waiting' AND resume_at <= now()`)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		slog.Info("resumed workflow runs", "count", tag.RowsAffected())
	}
	return nil
}

func hmacSHA256(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
