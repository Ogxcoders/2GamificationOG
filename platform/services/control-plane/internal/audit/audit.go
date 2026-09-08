// Package audit: append-only audit trail for config + identity mutations (§96).
// Every mutation handler records WHO did WHAT to WHICH object; failures to
// record never fail the user request (best-effort, but logged).
package audit

import (
        "context"
        "log"

        "universalengagement/control-plane/internal/db"
        "universalengagement/control-plane/internal/middleware"
)

// Record appends one audit entry. before/after may be nil.
func Record(ctx context.Context, pool *db.Pool, projectID, envID, action, subjectType, subjectID string, before, after any, reason string) {
        actorType := "system"
        actorID := "system"
        if a := middleware.ActorFrom(ctx); a != nil && a.KeyID != "" {
                // audit_log_actor_type_check allows: human|system|plugin|ai|automation.
                // API-key callers are automation principals.
                actorType = "automation"
                actorID = a.KeyID
        }

        var beforeJSON, afterJSON any
        if before != nil {
                beforeJSON = before
        }
        if after != nil {
                afterJSON = after
        }
        if _, err := pool.Exec(ctx, `
                INSERT INTO audit_log (project_id, environment_id, actor_type, actor_id, action,
                                       subject_type, subject_id, before, after, reason, request_id)
                VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
                projectID, envID, actorType, actorID, action,
                subjectType, subjectID, beforeJSON, afterJSON, reason, ""); err != nil {
                log.Printf("audit record failed (action=%s subject=%s): %v", action, subjectID, err)
        }
}
