// Package eventing: the event gateway (§10) — validation, idempotency,
// dedup, atomic batch ingestion — plus the in-process event bus.
package eventing

import (
        "context"
        "crypto/sha256"
        "encoding/hex"
        "encoding/json"
        "fmt"
        "strconv"
        "sync"
        "time"

        "universalengagement/control-plane/internal/db"

        "github.com/jackc/pgx/v5"
)

// CanonicalEvent mirrors the platform-wide event contract (§10).
type CanonicalEvent struct {
        EventID        string         `json:"event_id"`
        EventType      string         `json:"event_type"`
        EventVersion   int            `json:"event_version"`
        ProjectID      string         `json:"project_id"`
        EnvironmentID  string         `json:"environment_id"`
        ActorID        string         `json:"actor_id"`
        SubjectID      string         `json:"subject_id"`
        Source         string         `json:"source"`
        OccurredAt     string         `json:"occurred_at"`
        ReceivedAt     string         `json:"received_at"`
        CorrelationID  string         `json:"correlation_id"`
        CausationID    string         `json:"causation_id"`
        IdempotencyKey string         `json:"idempotency_key"`
        Payload        map[string]any `json:"payload"`
        Metadata       map[string]any `json:"metadata"`
}

// Normalize fills server-side defaults and validates (§10 requirements).
func (e *CanonicalEvent) Normalize() error {
        // Whether the client supplied occurred_at decides whether it is part
        // of the content fingerprint (server-defaulted timestamps would make
        // every replay unique).
        clientOccurredAt := e.OccurredAt != ""
        if e.EventID == "" {
                e.EventID = db.NewID("evt")
        }
        if e.EventType == "" {
                return db.ValidationError("event_type", "event_type is required (dotted names, e.g. lesson.completed)")
        }
        if e.ProjectID == "" {
                return db.ValidationError("project_id", "project_id is required")
        }
        if e.EnvironmentID == "" {
                return db.ValidationError("environment_id", "environment_id is required")
        }
        if e.EventVersion == 0 {
                e.EventVersion = 1
        }
        if e.Source == "" {
                e.Source = "server"
        }
        if e.OccurredAt == "" {
                e.OccurredAt = time.Now().UTC().Format(time.RFC3339)
        }
        if _, err := time.Parse(time.RFC3339, e.OccurredAt); err != nil {
                return db.ValidationError("occurred_at", "occurred_at must be an RFC3339 timestamp")
        }
        if e.ReceivedAt == "" {
                e.ReceivedAt = time.Now().UTC().Format(time.RFC3339)
        }
        if e.CorrelationID == "" {
                e.CorrelationID = db.NewID("corr")
        }
        if e.Payload == nil {
                e.Payload = map[string]any{}
        }
        // §11 idempotency: clients that send no idempotency_key get a
        // deterministic content fingerprint so batch replays are detected as
        // duplicates (truthful reporting), never double-processed.
        if e.IdempotencyKey == "" {
                e.IdempotencyKey = e.contentFingerprint(clientOccurredAt)
        }
        return nil
}

// contentFingerprint derives a stable SHA-256 fingerprint from the event's
// identity + content. occurred_at participates only when the client sent it.
// Go's json.Marshal sorts map keys, so payload serialization is canonical.
func (e *CanonicalEvent) contentFingerprint(includeOccurredAt bool) string {
        h := sha256.New()
        for _, part := range []string{
                e.ProjectID, e.EnvironmentID, e.EventType,
                strconv.Itoa(e.EventVersion), e.ActorID, e.SubjectID, e.Source,
        } {
                h.Write([]byte(part))
                h.Write([]byte{0})
        }
        if includeOccurredAt {
                h.Write([]byte(e.OccurredAt))
                h.Write([]byte{0})
        }
        if pb, err := json.Marshal(e.Payload); err == nil {
                h.Write(pb)
        }
        return "fp_" + hex.EncodeToString(h.Sum(nil))[:40]
}

// IngestResult reports per-event outcomes truthfully.
type IngestResult struct {
        EventID string `json:"event_id"`
        // "inserted" (new) | "duplicate" (idempotent replay — NOT reprocessed) | "rejected".
        Status string `json:"status"`
        Error  string `json:"error,omitempty"`
}

// IngestBatch atomically inserts events with ON CONFLICT semantics:
// duplicates are reported truthfully and are NOT reprocessed (§11
// idempotency; the atomic INSERT .. ON CONFLICT DO NOTHING pattern makes
// concurrent replays safe — only one insertion wins).
func IngestBatch(ctx context.Context, pool *db.Pool, events []*CanonicalEvent) ([]IngestResult, error) {
        results := make([]IngestResult, 0, len(events))

        tx, err := pool.Begin(ctx)
        if err != nil {
                return nil, err
        }
        defer func() { _ = tx.Rollback(ctx) }()

        for _, e := range events {
                if err := e.Normalize(); err != nil {
                        results = append(results, IngestResult{EventID: e.EventID, Status: "rejected", Error: err.Error()})
                        continue
                }
                var idemKey *string
                if e.IdempotencyKey != "" {
                        idemKey = &e.IdempotencyKey
                }
                inserted, existingID, err := insertEvent(ctx, tx, e, idemKey)
                if err != nil {
                        return nil, err
                }
                status := "duplicate"
                if inserted {
                        status = "inserted"
                        // §9 auto-provision: an actor's first ingested event
                        // creates their (anonymous) user row.
                        if e.ActorID != "" {
                                _, _ = tx.Exec(ctx, `
                                        INSERT INTO users (id, project_id, environment_id, anonymous)
                                        VALUES ($1,$2,$3,true)
                                        ON CONFLICT DO NOTHING`,
                                        e.ActorID, e.ProjectID, e.EnvironmentID)
                        }
                }
                // Truthful reporting: a fingerprint collision references the
                // ORIGINAL event id, not the freshly generated one.
                reportedID := e.EventID
                if !inserted && existingID != "" {
                        reportedID = existingID
                }
                results = append(results, IngestResult{EventID: reportedID, Status: status})
        }

        if err := tx.Commit(ctx); err != nil {
                return nil, err
        }
        return results, nil
}

// insertEvent performs the atomic insert; inserted=false on conflict, and
// existingID resolves the original event when the conflict came from the
// idempotency fingerprint (so callers can report the true event id).
func insertEvent(ctx context.Context, tx pgx.Tx, e *CanonicalEvent, idemKey *string) (bool, string, error) {
        var inserted bool
        err := tx.QueryRow(ctx, `
                INSERT INTO events (event_id, project_id, environment_id, event_type, event_version,
                                    actor_id, subject_id, source, occurred_at, received_at,
                                    correlation_id, causation_id, idempotency_key, payload, metadata)
                VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
                ON CONFLICT DO NOTHING
                RETURNING true`,
                e.EventID, e.ProjectID, e.EnvironmentID, e.EventType, e.EventVersion,
                e.ActorID, e.SubjectID, e.Source, e.OccurredAt, e.ReceivedAt,
                e.CorrelationID, e.CausationID, idemKey, jsonOrDefault(e.Payload), jsonOrDefault(e.Metadata)).
                Scan(&inserted)
        if err != nil {
                if err == pgx.ErrNoRows {
                        // conflict — duplicate replay; resolve the original event id
                        existing := ""
                        if idemKey != nil {
                            _ = tx.QueryRow(ctx, `
                                SELECT event_id FROM events
                                WHERE project_id=$1 AND environment_id=$2 AND idempotency_key=$3
                                ORDER BY occurred_at LIMIT 1`,
                                e.ProjectID, e.EnvironmentID, *idemKey).Scan(&existing)
                        }
                        return false, existing, nil
                }
                return false, "", fmt.Errorf("insert event %s: %w", e.EventID, err)
        }
        return inserted, "", nil
}

// GetEvent fetches one stored event.
func GetEvent(ctx context.Context, pool *db.Pool, projectID, envID, eventID string) (map[string]any, error) {
        var payload, metadata json.RawMessage
        var (
                eventType                                          string
                eventVersion                                       int
                actorID, subjectID, source, occurredAt, corr, caus string
        )
        var idem *string
        err := pool.QueryRow(ctx, `
                SELECT event_type, event_version, actor_id, subject_id, source,
                       to_char(occurred_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
                       correlation_id, causation_id, idempotency_key, payload, metadata
                FROM events WHERE event_id = $1 AND project_id = $2 AND environment_id = $3`,
                eventID, projectID, envID).
                Scan(&eventType, &eventVersion, &actorID, &subjectID, &source, &occurredAt, &corr, &caus, &idem, &payload, &metadata)
        if err != nil {
                return nil, db.NotFoundError("event not found")
        }
        row := map[string]any{
                "event_id": eventID, "event_type": eventType, "event_version": eventVersion,
                "actor_id": actorID, "subject_id": subjectID, "source": source,
                "occurred_at": occurredAt, "correlation_id": corr, "causation_id": caus,
                "payload": payload, "metadata": metadata,
        }
        if idem != nil {
                row["idempotency_key"] = *idem
        }
        return row, nil
}

// ListFilter for the event inspector (§72).
type ListFilter struct {
        EventType string
        ActorID   string
        Status    string
        Limit     int
        Offset    int
}

// ListEvents queries events by filters.
func ListEvents(ctx context.Context, pool *db.Pool, projectID, envID string, f ListFilter) ([]map[string]any, error) {
        limit := f.Limit
        if limit <= 0 || limit > 500 {
                limit = 50
        }
        rows, err := pool.Query(ctx, `
                SELECT event_id, event_type, actor_id, source,
                       to_char(occurred_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'), status, payload
                FROM events
                WHERE project_id = $1 AND environment_id = $2
                  AND ($3 = '' OR event_type = $3)
                  AND ($4 = '' OR actor_id = $4)
                  AND ($6 = '' OR status = $6)
                ORDER BY occurred_at DESC
                LIMIT $5 OFFSET $7`, projectID, envID, f.EventType, f.ActorID, limit, f.Status, f.Offset)
        if err != nil {
                return nil, err
        }
        defer rows.Close()
        var out []map[string]any
        for rows.Next() {
                var id, etype, actor, source, at, status string
                var payload json.RawMessage
                if err := rows.Scan(&id, &etype, &actor, &source, &at, &status, &payload); err != nil {
                        return nil, err
                }
                out = append(out, map[string]any{
                        "event_id": id, "event_type": etype, "actor_id": actor,
                        "source": source, "occurred_at": at, "status": status, "payload": payload,
                })
        }
        return out, rows.Err()
}

// MarkProcessed records processing status.
func MarkProcessed(ctx context.Context, pool *db.Pool, eventID string, failed bool, errMsg string) error {
        status := "processed"
        if failed {
                status = "failed"
        }
        _, err := pool.Exec(ctx, `UPDATE events SET status = $1, error = $2 WHERE event_id = $3`, status, errMsg, eventID)
        return err
}

// ── In-process event bus (§10). NATS in production; identical semantics
// for single-binary local dev (§86).

type Handler func(ctx context.Context, event *CanonicalEvent)

type Bus struct {
        mu       sync.RWMutex
        handlers map[string][]Handler
}

// NewBus creates the bus.
func NewBus() *Bus {
        return &Bus{handlers: map[string][]Handler{}}
}

// Subscribe registers a handler for an event type ("*" = all).
func (b *Bus) Subscribe(eventType string, h Handler) {
        b.mu.Lock()
        defer b.mu.Unlock()
        b.handlers[eventType] = append(b.handlers[eventType], h)
}

// Publish fans out (at-least-once; handlers must be idempotent).
func (b *Bus) Publish(ctx context.Context, e *CanonicalEvent) {
        b.mu.RLock()
        handlers := append([]Handler{}, b.handlers[e.EventType]...)
        wildcard := b.handlers["*"]
        b.mu.RUnlock()
        for _, h := range append(handlers, wildcard...) {
                h(ctx, e)
        }
}

// jsonOrDefault maps nil Go maps to an empty JSON object for NOT NULL columns.
func jsonOrDefault(m map[string]any) map[string]any {
        if m == nil {
                return map[string]any{}
        }
        return m
}
