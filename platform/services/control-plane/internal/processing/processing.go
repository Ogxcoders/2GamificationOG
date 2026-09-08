// Package processing: the event processing pipeline (§4 universal flow, §15
// atomicity, §11 idempotency, §70 decision trace).
//
//	event ingested → load actor state → engine evaluates → apply commands
//	atomically (single tx) → persist trace + state → outbox → mark processed
//
// The engine is pure; ALL durability decisions live here.
package processing

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"universalengagement/control-plane/internal/configsvc"
	"universalengagement/control-plane/internal/db"
	"universalengagement/control-plane/internal/engine"

	"github.com/jackc/pgx/v5"
)

// Deps wires the pipeline.
type Deps struct {
	Pool   *db.Pool
	Engine *engine.Client
}

// Result reports what happened for one event.
type Result struct {
	EventID         string                `json:"event_id"`
	Processed       bool                  `json:"processed"`
	Failed          bool                  `json:"failed"`
	Outcome         *engine.EngineOutcome `json:"outcome,omitempty"`
	AppliedCommands int                   `json:"applied_commands"`
	SkippedCommands int                   `json:"skipped_commands"`
	Error           string                `json:"error,omitempty"`
	DurationMS      int64                 `json:"duration_ms"`
}

// currencyPolicy mirrors the authored currency rules the ledger validator needs.
type currencyPolicy struct {
	AllowNegative bool
	Cap           int64
}

// runCtx carries per-event pipeline context (never shared across goroutines).
type runCtx struct {
	projectID  string
	envID      string
	eventID    string
	eventType  string
	userID     string
	currencies map[string]currencyPolicy
}

// ProcessEvent runs the full pipeline for one ingested event.
func ProcessEvent(ctx context.Context, deps Deps, projectID, envID, eventID string) (*Result, error) {
	start := time.Now()
	res := &Result{EventID: eventID}

	event, userID, err := loadEvent(ctx, deps.Pool, projectID, envID, eventID)
	if err != nil {
		return nil, err
	}

	// Engine call happens BEFORE the write tx (pure, no locks held).
	cfg, err := configsvc.CurrentEngineConfig(ctx, deps.Pool, projectID, envID)
	if err != nil {
		return nil, fmt.Errorf("load engine config: %w", err)
	}
	actorState, err := LoadActorState(ctx, deps.Pool, projectID, envID, userID)
	if err != nil {
		return nil, fmt.Errorf("load actor state: %w", err)
	}

	engineReq := &engine.ProcessRequest{
		Event:      event,
		Config:     cfg.Config,
		ActorState: actorState,
	}
	engineResp, err := deps.Engine.Process(ctx, engineReq)
	if err != nil {
		_ = markEvent(ctx, deps.Pool, eventID, "failed", err.Error())
		res.Failed = true
		res.Error = err.Error()
		res.DurationMS = msSince(start)
		return res, nil
	}
	if !engineResp.OK {
		_ = markEvent(ctx, deps.Pool, eventID, "failed", engineResp.Error)
		res.Failed = true
		res.Error = engineResp.Error
		res.DurationMS = msSince(start)
		return res, nil
	}

	run := &runCtx{
		projectID:  projectID,
		envID:      envID,
		eventID:    eventID,
		eventType:  strOf(event["event_type"]),
		userID:     userID,
		currencies: currencyPoliciesFrom(cfg.Config),
	}

	// Apply atomically.
	applied, skipped, aerr := applyOutcome(ctx, deps.Pool, run, engineResp.Outcome)
	if aerr != nil {
		_ = markEvent(ctx, deps.Pool, eventID, "failed", aerr.Error())
		res.Failed = true
		res.Error = aerr.Error()
		res.DurationMS = msSince(start)
		return res, nil
	}
	res.Outcome = engineResp.Outcome
	res.Processed = true
	res.AppliedCommands = applied
	res.SkippedCommands = skipped
	res.DurationMS = msSince(start)
	return res, nil
}

func msSince(t time.Time) int64 { return time.Since(t).Milliseconds() }

func currencyPoliciesFrom(cfg map[string]any) map[string]currencyPolicy {
	out := map[string]currencyPolicy{}
	currs, _ := cfg["currencies"].([]any)
	for _, c := range currs {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		p := currencyPolicy{}
		if an, ok := m["allow_negative"].(bool); ok {
			p.AllowNegative = an
		}
		if cap, ok := m["cap"].(float64); ok {
			p.Cap = int64(cap)
		}
		out[id] = p
	}
	return out
}

func loadEvent(ctx context.Context, pool *db.Pool, projectID, envID, eventID string) (map[string]any, string, error) {
	var payload json.RawMessage
	var eventType, actorID, subjectID, source, occurredAt, corrID, causID string
	var eventVersion int
	err := pool.QueryRow(ctx, `
                SELECT event_type, event_version, actor_id, subject_id, source,
                       to_char(occurred_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'), correlation_id, causation_id, payload
                FROM events WHERE event_id=$1 AND project_id=$2 AND environment_id=$3`,
		eventID, projectID, envID).
		Scan(&eventType, &eventVersion, &actorID, &subjectID, &source, &occurredAt, &corrID, &causID, &payload)
	if err != nil {
		return nil, "", db.NotFoundError("event not found in this environment")
	}
	var payloadMap map[string]any
	_ = json.Unmarshal(payload, &payloadMap)
	if payloadMap == nil {
		payloadMap = map[string]any{}
	}
	return map[string]any{
		"event_id":       eventID,
		"project_id":     projectID,
		"environment_id": envID,
		"event_type":     eventType,
		"event_version":  eventVersion,
		"actor_id":       actorID,
		"subject_id":     subjectID,
		"source":         source,
		"occurred_at":    occurredAt,
		"correlation_id": corrID,
		"causation_id":   causID,
		"payload":        payloadMap,
	}, actorID, nil
}

func markEvent(ctx context.Context, pool *db.Pool, eventID, status, errMsg string) error {
	_, err := pool.Exec(ctx, `UPDATE events SET status=$2, error=$3 WHERE event_id=$1`, eventID, status, errMsg)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// Actor state loading (runtime tables → engine ActorState JSON)
// ─────────────────────────────────────────────────────────────────────────────

// LoadActorState reads every runtime projection for one user and assembles
// the engine-facing state snapshot.
func LoadActorState(ctx context.Context, pool *db.Pool, projectID, envID, userID string) (map[string]any, error) {
	state := map[string]any{
		"user_id":      userID,
		"tracks":       map[string]any{},
		"rule_state":   map[string]any{"last_fired": map[string]any{}, "fire_counts": map[string]any{}},
		"challenges":   map[string]any{},
		"streaks":      map[string]any{},
		"achievements": map[string]any{},
		"leaderboards": map[string]any{},
		"variables":    map[string]any{},
	}

	// XP tracks.
	rows, err := pool.Query(ctx, `
                SELECT track, xp, level, highest_level FROM xp_tracks
                WHERE project_id=$1 AND environment_id=$2 AND user_id=$3`, projectID, envID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tracks := map[string]any{}
	for rows.Next() {
		var track string
		var xp, level, highest int64
		if err := rows.Scan(&track, &xp, &level, &highest); err != nil {
			return nil, err
		}
		tracks[track] = map[string]any{"track": track, "xp": xp, "level": level, "highest_level": highest}
	}
	state["tracks"] = tracks

	// Rule state.
	var ruleRaw json.RawMessage
	if err := pool.QueryRow(ctx, `
                SELECT state FROM rule_state WHERE project_id=$1 AND environment_id=$2 AND user_id=$3`,
		projectID, envID, userID).Scan(&ruleRaw); err == nil {
		var rs map[string]any
		if json.Unmarshal(ruleRaw, &rs) == nil && rs != nil {
			if lf, ok := rs["last_fired"].(map[string]any); ok {
				state["rule_state"].(map[string]any)["last_fired"] = lf
			}
			if fc, ok := rs["fire_counts"].(map[string]any); ok {
				state["rule_state"].(map[string]any)["fire_counts"] = fc
			}
		}
	}

	// Challenges: latest row per challenge (window-keyed rows stay in the
	// table; the engine works on the current window).
	chRows, err := pool.Query(ctx, `
                SELECT DISTINCT ON (challenge_id) challenge_id, progress, target, status, window_key, completions, granted_rewards
                FROM challenge_progress
                WHERE project_id=$1 AND environment_id=$2 AND user_id=$3
                ORDER BY challenge_id, updated_at DESC`, projectID, envID, userID)
	if err != nil {
		return nil, err
	}
	defer chRows.Close()
	challenges := map[string]any{}
	for chRows.Next() {
		var id, status, windowKey string
		var progress, target, completions int64
		var granted []string
		if err := chRows.Scan(&id, &progress, &target, &status, &windowKey, &completions, &granted); err != nil {
			return nil, err
		}
		if granted == nil {
			granted = []string{}
		}
		challenges[id] = map[string]any{
			"challenge_id": id, "progress": progress, "target": target,
			"status": status, "window_key": windowKey, "completions": completions,
			"granted_rewards": granted,
		}
	}
	state["challenges"] = challenges

	// Streaks.
	stRows, err := pool.Query(ctx, `
                SELECT streak_id, current, best, last_window_key, freezes_used, active FROM streak_state
                WHERE project_id=$1 AND environment_id=$2 AND user_id=$3`, projectID, envID, userID)
	if err != nil {
		return nil, err
	}
	defer stRows.Close()
	streaks := map[string]any{}
	for stRows.Next() {
		var id, lastKey string
		var current, best int64
		var freezes int
		var active bool
		if err := stRows.Scan(&id, &current, &best, &lastKey, &freezes, &active); err != nil {
			return nil, err
		}
		streaks[id] = map[string]any{
			"streak_id": id, "current": current, "best": best,
			"last_window_key": lastKey, "freezes_used": freezes, "active": active,
		}
	}
	state["streaks"] = streaks

	// Achievements: unlocked epoch ms.
	achRows, err := pool.Query(ctx, `
                SELECT achievement_id, EXTRACT(EPOCH FROM unlocked_at)*1000 FROM achievement_unlocks
                WHERE project_id=$1 AND environment_id=$2 AND user_id=$3`, projectID, envID, userID)
	if err != nil {
		return nil, err
	}
	defer achRows.Close()
	achievements := map[string]any{}
	for achRows.Next() {
		var id string
		var ms float64
		if err := achRows.Scan(&id, &ms); err != nil {
			return nil, err
		}
		achievements[id] = int64(ms)
	}
	state["achievements"] = achievements

	// Leaderboard scores (cumulative 'ever' window).
	lbRows, err := pool.Query(ctx, `
                SELECT leaderboard_id, score FROM leaderboard_entries
                WHERE project_id=$1 AND environment_id=$2 AND user_id=$3 AND window_key='ever'`,
		projectID, envID, userID)
	if err != nil {
		return nil, err
	}
	defer lbRows.Close()
	lbs := map[string]any{}
	for lbRows.Next() {
		var id string
		var score int64
		if err := lbRows.Scan(&id, &score); err != nil {
			return nil, err
		}
		lbs[id] = score
	}
	state["leaderboards"] = lbs

	// Variables.
	varRows, err := pool.Query(ctx, `
                SELECT key, value FROM user_variables
                WHERE project_id=$1 AND environment_id=$2 AND user_id=$3`, projectID, envID, userID)
	if err != nil {
		return nil, err
	}
	defer varRows.Close()
	vars := map[string]any{}
	for varRows.Next() {
		var key string
		var raw json.RawMessage
		if err := varRows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		var v any
		_ = json.Unmarshal(raw, &v)
		vars[key] = v
	}
	state["variables"] = vars

	return state, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Atomic outcome application
// ─────────────────────────────────────────────────────────────────────────────

// applyOutcome applies the engine's commands + state projection in ONE
// transaction. Returns (applied, skipped, error).
func applyOutcome(ctx context.Context, pool *db.Pool, run *runCtx, outcome *engine.EngineOutcome) (int, int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the event row: concurrent processors of the same event serialize
	// here; the second observes 'processed' and dedups.
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM events WHERE event_id=$1 FOR UPDATE`, run.eventID).Scan(&status); err != nil {
		return 0, 0, fmt.Errorf("lock event: %w", err)
	}
	if status == "processed" {
		return 0, len(outcome.Commands), nil // replay: nothing to do
	}

	applied, skipped := 0, 0
	for i := range outcome.Commands {
		cmd := &outcome.Commands[i]
		ok, err := applyCommand(ctx, tx, run, cmd)
		if err != nil {
			return applied, skipped, fmt.Errorf("command %s (%s): %w", cmd.CommandID, cmd.Kind.Type, err)
		}
		if ok {
			applied++
		} else {
			skipped++
		}
	}

	// Persist the projected state snapshot (authoritative for reads).
	if err := persistState(ctx, tx, run, outcome); err != nil {
		return applied, skipped, fmt.Errorf("persist state: %w", err)
	}

	// Persist the decision trace (§70).
	if err := persistTrace(ctx, tx, run, outcome); err != nil {
		return applied, skipped, fmt.Errorf("persist trace: %w", err)
	}

	// Outbox: notifications + webhook fan-out happen post-commit (at-least-once).
	if err := enqueueOutbox(ctx, tx, run, outcome); err != nil {
		return applied, skipped, fmt.Errorf("outbox: %w", err)
	}

	if _, err := tx.Exec(ctx, `UPDATE events SET status='processed', error=NULL WHERE event_id=$1`, run.eventID); err != nil {
		return applied, skipped, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return applied, skipped, nil
}

// applyCommand applies ONE command. Returns (applied, error); applied=false
// means a duplicate (idempotent replay) — not an error.
func applyCommand(ctx context.Context, tx pgx.Tx, run *runCtx, cmd *engine.Command) (bool, error) {
	switch cmd.Kind.Type {
	case "award_xp":
		return true, upsertTrack(ctx, tx, run, cmd)
	case "add_currency":
		return applyLedger(ctx, tx, run, cmd, cmd.Kind.Amount)
	case "spend_currency":
		if cmd.Kind.Amount < 0 {
			return false, fmt.Errorf("spend_currency amount must be positive (got %d)", cmd.Kind.Amount)
		}
		return applyLedger(ctx, tx, run, cmd, -cmd.Kind.Amount)
	case "grant_item":
		return true, applyGrantItem(ctx, tx, run, cmd)
	case "grant_reward":
		return true, applyGrantReward(ctx, tx, run, cmd)
	case "progress_challenge", "start_challenge", "complete_challenge", "update_streak", "set_variable":
		// Covered by the state projection below — commands document the change.
		return true, nil
	case "unlock_achievement":
		return true, applyUnlockAchievement(ctx, tx, run, cmd)
	case "update_leaderboard":
		return true, applyLeaderboard(ctx, tx, run, cmd)
	case "grant_entitlement":
		return applyGrantEntitlement(ctx, tx, run, cmd)
	case "notify":
		return true, applyNotify(ctx, tx, run, cmd)
	case "show_paywall":
		return true, applyPaywallDecision(ctx, tx, run, cmd)
	case "start_workflow":
		return true, applyStartWorkflow(ctx, tx, run, cmd)
	case "emit_event":
		return true, applyEmitEvent(ctx, tx, run, cmd)
	case "call_webhook":
		return true, applyCallWebhook(ctx, tx, run, cmd)
	default:
		// Unknown command types are skipped (forward compatibility) — the
		// trace records exactly what was not applied.
		return false, nil
	}
}

// ── XP ──

func upsertTrack(ctx context.Context, tx pgx.Tx, run *runCtx, cmd *engine.Command) error {
	track := orDefault(cmd.Kind.Track, "default")
	// Final xp/level come from the engine's state projection; here we only
	// guarantee the row exists for the projection to update.
	_, err := tx.Exec(ctx, `
                INSERT INTO xp_tracks (project_id, environment_id, user_id, track, xp, level, highest_level, updated_at)
                VALUES ($1,$2,$3,$4,0,0,0,now())
                ON CONFLICT (project_id, environment_id, user_id, track) DO NOTHING`,
		run.projectID, run.envID, run.userID, track)
	return err
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ── Economy (append-only ledger + NET validation, §27/§204) ──

func applyLedger(ctx context.Context, tx pgx.Tx, run *runCtx, cmd *engine.Command, signedAmount int64) (bool, error) {
	currency := orDefault(cmd.Kind.Currency, "default")

	// Append-only entry; UNIQUE (project, env, source, reference) makes
	// replays idempotent.
	tag, err := tx.Exec(ctx, `
                INSERT INTO ledger_entries (entry_id, project_id, environment_id, wallet_user_id, currency, amount, source, reason, reference, correlation_id)
                VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
                ON CONFLICT (project_id, environment_id, source, reference) DO NOTHING`,
		db.NewID("led"), run.projectID, run.envID, run.userID, currency, signedAmount, cmd.Source, cmd.Explanation, cmd.IdempotencyKey, cmd.EventID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil // duplicate — already applied
	}

	// Wallet projection + NET validation (two-pass, §204): balance is derived
	// from the ledger, never incremented blindly.
	var balance int64
	if err := tx.QueryRow(ctx, `
                SELECT COALESCE(SUM(amount),0) FROM ledger_entries
                WHERE project_id=$1 AND environment_id=$2 AND wallet_user_id=$3 AND currency=$4`,
		run.projectID, run.envID, run.userID, currency).Scan(&balance); err != nil {
		return false, err
	}

	policy := run.currencies[currency]
	if balance < 0 && !policy.AllowNegative {
		return false, fmt.Errorf("insufficient `%s` balance: NET %d after '%s' — transaction rolled back", currency, balance, cmd.Explanation)
	}
	if policy.Cap > 0 && balance > policy.Cap {
		return false, fmt.Errorf("`%s` cap exceeded: NET %d > cap %d — transaction rolled back", currency, balance, policy.Cap)
	}

	if _, err := tx.Exec(ctx, `
                INSERT INTO wallets (project_id, environment_id, user_id, currency, balance, updated_at)
                VALUES ($1,$2,$3,$4,$5,now())
                ON CONFLICT (project_id, environment_id, user_id, currency)
                DO UPDATE SET balance = $5, updated_at = now()`,
		run.projectID, run.envID, run.userID, currency, balance); err != nil {
		return false, err
	}
	return true, nil
}

// ── Inventory ──

func applyGrantItem(ctx context.Context, tx pgx.Tx, run *runCtx, cmd *engine.Command) error {
	qty := cmd.Kind.Quantity
	if qty <= 0 {
		qty = 1
	}
	_, err := tx.Exec(ctx, `
                INSERT INTO inventory_items (id, project_id, environment_id, user_id, item_id, quantity, metadata)
                VALUES ($1,$2,$3,$4,$5,$6,'{}'::jsonb)
                ON CONFLICT (project_id, environment_id, user_id, item_id)
                DO UPDATE SET quantity = inventory_items.quantity + $6, acquired_at = now()`,
		db.NewID("inv"), run.projectID, run.envID, run.userID, cmd.Kind.Item, qty)
	return err
}

// ── Reward grants (§205 unique logical grant) ──

func applyGrantReward(ctx context.Context, tx pgx.Tx, run *runCtx, cmd *engine.Command) error {
	_, err := tx.Exec(ctx, `
                INSERT INTO reward_grants (grant_id, project_id, environment_id, user_id, reward_source, source_id, reward_type, quantity)
                VALUES ($1,$2,$3,$4,$5,$6,'reward',1)
                ON CONFLICT (project_id, environment_id, user_id, reward_source, source_id, reward_type) DO NOTHING`,
		db.NewID("grant"), run.projectID, run.envID, run.userID, cmd.Source, cmd.IdempotencyKey)
	return err
}

// ── Achievements ──

func applyUnlockAchievement(ctx context.Context, tx pgx.Tx, run *runCtx, cmd *engine.Command) error {
	_, err := tx.Exec(ctx, `
                INSERT INTO achievement_unlocks (project_id, environment_id, user_id, achievement_id)
                VALUES ($1,$2,$3,$4)
                ON CONFLICT (project_id, environment_id, user_id, achievement_id) DO NOTHING`,
		run.projectID, run.envID, run.userID, cmd.Kind.Achievement)
	return err
}

// ── Leaderboards (deterministic tie key: score, achieved_at, user_id) ──

func applyLeaderboard(ctx context.Context, tx pgx.Tx, run *runCtx, cmd *engine.Command) error {
	windowKey := orDefault(cmd.Kind.WindowKey, "ever")
	delta := cmd.Kind.Delta
	// Score column: engine computes the new total when it has state; delta
	// otherwise. COALESCE merge keeps the row authoritative.
	_, err := tx.Exec(ctx, `
                INSERT INTO leaderboard_entries (id, project_id, environment_id, leaderboard_id, user_id, score, achieved_at, window_key)
                VALUES ($1,$2,$3,$4,$5,$6,now(),$7)
                ON CONFLICT (project_id, environment_id, leaderboard_id, user_id, window_key)
                DO UPDATE SET score = leaderboard_entries.score + $6, achieved_at = now(), updated_at = now()`,
		db.NewID("lbe"), run.projectID, run.envID, cmd.Kind.Leaderboard, run.userID, delta, windowKey)
	return err
}

// ── Entitlements (revoked_at pattern, §40) ──

func applyGrantEntitlement(ctx context.Context, tx pgx.Tx, run *runCtx, cmd *engine.Command) (bool, error) {
	// Dedup identity (§205): the command's idempotency key guards replays.
	tag, err := tx.Exec(ctx, `
                INSERT INTO reward_grants (grant_id, project_id, environment_id, user_id, reward_source, source_id, reward_type, quantity)
                VALUES ($1,$2,$3,$4,$5,$6,'entitlement',1)
                ON CONFLICT (project_id, environment_id, user_id, reward_source, source_id, reward_type) DO NOTHING`,
		db.NewID("grant"), run.projectID, run.envID, run.userID, cmd.Source, cmd.IdempotencyKey)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil // duplicate
	}
	var expiresAt any
	if cmd.Kind.Duration > 0 {
		expiresAt = time.Now().Add(time.Duration(cmd.Kind.Duration) * time.Second)
	}
	// Re-granting an active entitlement extends rather than duplicates.
	tag, err = tx.Exec(ctx, `
                UPDATE entitlements SET expires_at = $5, quantity = entitlements.quantity + 1
                WHERE project_id=$1 AND environment_id=$2 AND user_id=$3 AND entitlement=$4
                  AND revoked_at IS NULL`,
		run.projectID, run.envID, run.userID, cmd.Kind.Entitlement, expiresAt)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		_, err = tx.Exec(ctx, `
                        INSERT INTO entitlements (id, project_id, environment_id, user_id, entitlement, source, expires_at)
                        VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			db.NewID("ent"), run.projectID, run.envID, run.userID, cmd.Kind.Entitlement, cmd.Source, expiresAt)
	}
	return true, err
}

// ── Notifications (dedup via unique key, §75) ──

func applyNotify(ctx context.Context, tx pgx.Tx, run *runCtx, cmd *engine.Command) error {
	params := cmd.Kind.Params
	if params == nil {
		params = map[string]string{}
	}
	dedup := cmd.IdempotencyKey
	_, err := tx.Exec(ctx, `
                INSERT INTO notifications (id, project_id, environment_id, user_id, template, params, channel, dedup_key)
                VALUES ($1,$2,$3,$4,$5,$6,'in_app',$7)
                ON CONFLICT (project_id, environment_id, user_id, dedup_key) DO NOTHING`,
		db.NewID("note"), run.projectID, run.envID, run.userID, cmd.Kind.Template, params, dedup)
	return err
}

// ── Paywall decisions (§43) ──

func applyPaywallDecision(ctx context.Context, tx pgx.Tx, run *runCtx, cmd *engine.Command) error {
	_, err := tx.Exec(ctx, `
                INSERT INTO paywall_decisions (id, project_id, environment_id, user_id, paywall_id, event_id)
                VALUES ($1,$2,$3,$4,$5,$6)`,
		db.NewID("pwd"), run.projectID, run.envID, run.userID, cmd.Kind.Paywall, run.eventID)
	return err
}

// ── Workflows (§16) ──

func applyStartWorkflow(ctx context.Context, tx pgx.Tx, run *runCtx, cmd *engine.Command) error {
	_, err := tx.Exec(ctx, `
                INSERT INTO workflow_runs (execution_id, project_id, environment_id, workflow_id, trigger_event_id, actor_id, status, state)
                VALUES ($1,$2,$3,$4,$5,$6,'running','{}'::jsonb)
                ON CONFLICT (execution_id) DO NOTHING`,
		db.NewID("wfr"), run.projectID, run.envID, cmd.Kind.Workflow, run.eventID, run.userID)
	return err
}

// ── Emitted events (§10: engine-emitted events re-enter the pipeline) ──

func applyEmitEvent(ctx context.Context, tx pgx.Tx, run *runCtx, cmd *engine.Command) error {
	payload := cmd.Kind.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	newID := db.NewID("evt")
	_, err := tx.Exec(ctx, `
                INSERT INTO events (event_id, project_id, environment_id, event_type, actor_id, source, occurred_at, correlation_id, causation_id, payload, status)
                VALUES ($1,$2,$3,$4,$5,'engine',$6,$7,$8,$9,'received')
                ON CONFLICT (event_id) DO NOTHING`,
		newID, run.projectID, run.envID, cmd.Kind.EventType, run.userID,
		time.Now().UTC(), run.eventID, run.eventID, payload)
	return err
}

// ── Direct webhook calls (§99) ──

func applyCallWebhook(ctx context.Context, tx pgx.Tx, run *runCtx, cmd *engine.Command) error {
	body := map[string]any{
		"event_id":   run.eventID,
		"source":     cmd.Source,
		"command_id": cmd.CommandID,
		"endpoint":   cmd.Kind.Endpoint,
		"user_id":    run.userID,
	}
	_, err := tx.Exec(ctx, `
                INSERT INTO outbox (project_id, environment_id, event_id, topic, payload)
                VALUES ($1,$2,$3,'webhook.call',$4)`,
		run.projectID, run.envID, run.eventID, body)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// State projection persistence (engine state → runtime tables)
// ─────────────────────────────────────────────────────────────────────────────

func persistState(ctx context.Context, tx pgx.Tx, run *runCtx, outcome *engine.EngineOutcome) error {
	state := outcome.State
	if state == nil {
		return nil
	}

	// XP tracks (absolute values from the engine projection).
	if tracks, ok := state["tracks"].(map[string]any); ok {
		for trackID, tv := range tracks {
			tm, ok := tv.(map[string]any)
			if !ok {
				continue
			}
			xp := i64Of(tm["xp"])
			level := i64Of(tm["level"])
			highest := i64Of(tm["highest_level"])
			_, err := tx.Exec(ctx, `
                                INSERT INTO xp_tracks (project_id, environment_id, user_id, track, xp, level, highest_level, updated_at)
                                VALUES ($1,$2,$3,$4,$5,$6,$7,now())
                                ON CONFLICT (project_id, environment_id, user_id, track)
                                DO UPDATE SET xp=$5, level=$6, highest_level=$7, updated_at=now()`,
				run.projectID, run.envID, run.userID, trackID, xp, level, highest)
			if err != nil {
				return fmt.Errorf("track %s: %w", trackID, err)
			}
		}
	}

	// Rule state (cooldowns + frequency).
	if rs, ok := state["rule_state"].(map[string]any); ok {
		_, err := tx.Exec(ctx, `
                        INSERT INTO rule_state (project_id, environment_id, user_id, state, updated_at)
                        VALUES ($1,$2,$3,$4,now())
                        ON CONFLICT (project_id, environment_id, user_id)
                        DO UPDATE SET state=$4, updated_at=now()`,
			run.projectID, run.envID, run.userID, rs)
		if err != nil {
			return fmt.Errorf("rule_state: %w", err)
		}
	}

	// Challenges (window-keyed upsert).
	if challenges, ok := state["challenges"].(map[string]any); ok {
		for _, cv := range challenges {
			cm, ok := cv.(map[string]any)
			if !ok {
				continue
			}
			challengeID, _ := cm["challenge_id"].(string)
			if challengeID == "" {
				continue
			}
			windowKey, _ := cm["window_key"].(string)
			if windowKey == "" {
				windowKey = "ever"
			}
			granted := stringSliceOf(cm["granted_rewards"])
			_, err := tx.Exec(ctx, `
                                INSERT INTO challenge_progress (id, project_id, environment_id, user_id, challenge_id, progress, target, status, window_key, completions, granted_rewards, updated_at)
                                VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,now())
                                ON CONFLICT (project_id, environment_id, user_id, challenge_id, window_key)
                                DO UPDATE SET progress=$6, target=$7, status=$8, completions=$10, granted_rewards=$11, updated_at=now()`,
				db.NewID("chp"), run.projectID, run.envID, run.userID, challengeID,
				i64Of(cm["progress"]), i64Of(cm["target"]), orDefault(strOf(cm["status"]), "in_progress"),
				windowKey, i64Of(cm["completions"]), granted)
			if err != nil {
				return fmt.Errorf("challenge %s: %w", challengeID, err)
			}
		}
	}

	// Streaks.
	if streaks, ok := state["streaks"].(map[string]any); ok {
		for _, sv := range streaks {
			sm, ok := sv.(map[string]any)
			if !ok {
				continue
			}
			streakID, _ := sm["streak_id"].(string)
			if streakID == "" {
				continue
			}
			_, err := tx.Exec(ctx, `
                                INSERT INTO streak_state (project_id, environment_id, user_id, streak_id, current, best, last_window_key, freezes_used, active, updated_at)
                                VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now())
                                ON CONFLICT (project_id, environment_id, user_id, streak_id)
                                DO UPDATE SET current=$5, best=$6, last_window_key=$7, freezes_used=$8, active=$9, updated_at=now()`,
				run.projectID, run.envID, run.userID, streakID,
				i64Of(sm["current"]), i64Of(sm["best"]), strOf(sm["last_window_key"]),
				int(i64Of(sm["freezes_used"])), boolOf(sm["active"]))
			if err != nil {
				return fmt.Errorf("streak %s: %w", streakID, err)
			}
		}
	}

	// Achievements.
	if achievements, ok := state["achievements"].(map[string]any); ok {
		for achID, ms := range achievements {
			_, err := tx.Exec(ctx, `
                                INSERT INTO achievement_unlocks (project_id, environment_id, user_id, achievement_id, unlocked_at)
                                VALUES ($1,$2,$3,$4, to_timestamp($5/1000.0))
                                ON CONFLICT (project_id, environment_id, user_id, achievement_id) DO NOTHING`,
				run.projectID, run.envID, run.userID, achID, i64Of(ms))
			if err != nil {
				return fmt.Errorf("achievement %s: %w", achID, err)
			}
		}
	}

	// Leaderboard cumulative scores (ever window).
	if lbs, ok := state["leaderboards"].(map[string]any); ok {
		for lbID, score := range lbs {
			_, err := tx.Exec(ctx, `
                                INSERT INTO leaderboard_entries (id, project_id, environment_id, leaderboard_id, user_id, score, window_key)
                                VALUES ($1,$2,$3,$4,$5,$6,'ever')
                                ON CONFLICT (project_id, environment_id, leaderboard_id, user_id, window_key)
                                DO UPDATE SET score=$6, achieved_at=now(), updated_at=now()`,
				db.NewID("lbe"), run.projectID, run.envID, lbID, run.userID, i64Of(score))
			if err != nil {
				return fmt.Errorf("leaderboard %s: %w", lbID, err)
			}
		}
	}

	// Variables.
	if vars, ok := state["variables"].(map[string]any); ok {
		for key, value := range vars {
			_, err := tx.Exec(ctx, `
                                INSERT INTO user_variables (project_id, environment_id, user_id, key, value, updated_at)
                                VALUES ($1,$2,$3,$4,$5,now())
                                ON CONFLICT (project_id, environment_id, user_id, key)
                                DO UPDATE SET value=$5, updated_at=now()`,
				run.projectID, run.envID, run.userID, key, value)
			if err != nil {
				return fmt.Errorf("variable %s: %w", key, err)
			}
		}
	}

	return nil
}

func persistTrace(ctx context.Context, tx pgx.Tx, run *runCtx, outcome *engine.EngineOutcome) error {
	nodes := outcome.Trace.Nodes
	if nodes == nil {
		nodes = []engine.TraceNode{}
	}
	_, err := tx.Exec(ctx, `
                INSERT INTO decision_traces (trace_id, project_id, environment_id, event_id, actor_id, config_version, nodes)
                VALUES ($1,$2,$3,$4,$5,$6,$7)
                ON CONFLICT (trace_id) DO NOTHING`,
		outcome.Trace.TraceID, run.projectID, run.envID, run.eventID, run.userID,
		outcome.Trace.ConfigVersion, nodes)
	return err
}

func enqueueOutbox(ctx context.Context, tx pgx.Tx, run *runCtx, outcome *engine.EngineOutcome) error {
	// One event.processed topic entry per event (fan-out to webhooks by the
	// publisher), plus achievement/challenge milestones as separate topics.
	body := map[string]any{
		"event_id":      run.eventID,
		"event_type":    run.eventType,
		"user_id":       run.userID,
		"rules":         outcome.RulesMatched,
		"level_ups":     outcome.LevelUps,
		"xp_deltas":     outcome.XpDeltas,
		"wallet_deltas": outcome.WalletDeltas,
	}
	_, err := tx.Exec(ctx, `
                INSERT INTO outbox (project_id, environment_id, event_id, topic, payload)
                VALUES ($1,$2,$3,'event.processed',$4)`,
		run.projectID, run.envID, run.eventID, body)
	return err
}

// ── JSON coercion helpers ─────────────────────────────────────────────────────

func i64Of(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolOf(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func stringSliceOf(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
