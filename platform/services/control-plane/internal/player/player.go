// Package player: runtime read APIs for SDK clients + direct economy
// operations (§27 authorized writes bypass rules when explicitly allowed).
package player

import (
	"context"
	"encoding/json"
	"fmt"

	"universalengagement/control-plane/internal/db"
)

// FullState is the complete player profile snapshot for one user.
type FullState struct {
	User         map[string]any   `json:"user"`
	Tracks       []map[string]any `json:"tracks"`
	Wallets      []map[string]any `json:"wallets"`
	Inventory    []map[string]any `json:"inventory"`
	Challenges   []map[string]any `json:"challenges"`
	Achievements []map[string]any `json:"achievements"`
	Streaks      []map[string]any `json:"streaks"`
	Entitlements []map[string]any `json:"entitlements"`
	Leaderboards []map[string]any `json:"leaderboards"`
}

// GetFullState loads everything a client renders from one call.
func GetFullState(ctx context.Context, pool *db.Pool, projectID, envID, userID string) (*FullState, error) {
	st := &FullState{User: map[string]any{}}

	// User row (may not exist yet — anonymous on-the-fly users).
	var (
		anonymous   bool
		displayName string
		attrs       json.RawMessage
		createdAt   string
	)
	_ = pool.QueryRow(ctx, `
		SELECT anonymous, COALESCE(display_name,''), attributes,
		       to_char(created_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM users WHERE project_id=$1 AND environment_id=$2 AND id=$3`,
		projectID, envID, userID).
		Scan(&anonymous, &displayName, &attrs, &createdAt)
	var attrMap map[string]any
	_ = json.Unmarshal(attrs, &attrMap)
	if attrMap == nil {
		attrMap = map[string]any{}
	}
	st.User = map[string]any{
		"user_id":      userID,
		"anonymous":    anonymous,
		"display_name": displayName,
		"attributes":   attrMap,
		"created_at":   createdAt,
	}

	st.Tracks = queryMaps(ctx, pool, `
		SELECT jsonb_build_object('track', track, 'xp', xp, 'level', level,
		       'highest_level', highest_level,
		       'updated_at', to_char(updated_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		FROM xp_tracks WHERE project_id=$1 AND environment_id=$2 AND user_id=$3
		ORDER BY track`, projectID, envID, userID)

	st.Wallets = queryMaps(ctx, pool, `
		SELECT jsonb_build_object('currency', currency, 'balance', balance,
		       'updated_at', to_char(updated_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		FROM wallets WHERE project_id=$1 AND environment_id=$2 AND user_id=$3
		ORDER BY currency`, projectID, envID, userID)

	st.Inventory = queryMaps(ctx, pool, `
		SELECT jsonb_build_object('item_id', item_id, 'quantity', quantity,
		       'acquired_at', to_char(acquired_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		FROM inventory_items WHERE project_id=$1 AND environment_id=$2 AND user_id=$3
		ORDER BY item_id`, projectID, envID, userID)

	st.Challenges = queryMaps(ctx, pool, `
		SELECT jsonb_build_object('challenge_id', challenge_id, 'progress', progress,
		       'target', target, 'status', status, 'window_key', window_key,
		       'completions', completions,
		       'updated_at', to_char(updated_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		FROM (
		  SELECT DISTINCT ON (challenge_id) * FROM challenge_progress
		  WHERE project_id=$1 AND environment_id=$2 AND user_id=$3
		  ORDER BY challenge_id, updated_at DESC
		) latest ORDER BY challenge_id`, projectID, envID, userID)

	st.Achievements = queryMaps(ctx, pool, `
		SELECT jsonb_build_object('achievement_id', achievement_id,
		       'unlocked_at', to_char(unlocked_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		FROM achievement_unlocks WHERE project_id=$1 AND environment_id=$2 AND user_id=$3
		ORDER BY unlocked_at DESC`, projectID, envID, userID)

	st.Streaks = queryMaps(ctx, pool, `
		SELECT jsonb_build_object('streak_id', streak_id, 'current', current, 'best', best,
		       'active', active,
		       'updated_at', to_char(updated_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		FROM streak_state WHERE project_id=$1 AND environment_id=$2 AND user_id=$3
		ORDER BY streak_id`, projectID, envID, userID)

	st.Entitlements = queryMaps(ctx, pool, `
		SELECT jsonb_build_object('entitlement', entitlement, 'source', source,
		       'expires_at', to_char(expires_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		FROM entitlements WHERE project_id=$1 AND environment_id=$2 AND user_id=$3
		  AND revoked_at IS NULL ORDER BY entitlement`, projectID, envID, userID)

	// Leaderboard positions (ever window) with deterministic rank.
	st.Leaderboards = queryMaps(ctx, pool, `
		SELECT jsonb_build_object('leaderboard_id', e.leaderboard_id, 'score', e.score,
		       'rank', (SELECT COUNT(*)+1 FROM leaderboard_entries x
		                WHERE x.project_id=e.project_id AND x.environment_id=e.environment_id
		                  AND x.leaderboard_id=e.leaderboard_id AND x.window_key=e.window_key
		                  AND (x.score > e.score OR (x.score = e.score AND x.achieved_at < e.achieved_at)
		                       OR (x.score = e.score AND x.achieved_at = e.achieved_at AND x.user_id < e.user_id))))
		FROM leaderboard_entries e
		WHERE e.project_id=$1 AND e.environment_id=$2 AND e.user_id=$3 AND e.window_key='ever'
		ORDER BY e.leaderboard_id`, projectID, envID, userID)

	return st, nil
}

func queryMaps(ctx context.Context, pool *db.Pool, q string, args ...any) []map[string]any {
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Leaderboard reads (§29/§206 deterministic rank with ties)
// ─────────────────────────────────────────────────────────────────────────────

// LeaderboardPage is one board view: top entries + optional caller position.
type LeaderboardPage struct {
	LeaderboardID string           `json:"leaderboard_id"`
	WindowKey     string           `json:"window_key"`
	Direction     string           `json:"direction"`
	Top           []map[string]any `json:"top"`
	Me            *map[string]any  `json:"me,omitempty"`
	Total         int              `json:"total_entries"`
}

// rankExpr is the standard-competition-rank subquery: entries ahead of `e`
// under the deterministic tie key (score, achieved_at, user_id) — ties share
// rank and NEVER use entry ids.
const rankExpr = `(SELECT COUNT(*)+1 FROM leaderboard_entries x
 WHERE x.project_id=e.project_id AND x.environment_id=e.environment_id
   AND x.leaderboard_id=e.leaderboard_id AND x.window_key=e.window_key
   AND (x.score > e.score OR (x.score = e.score AND x.achieved_at < e.achieved_at)
        OR (x.score = e.score AND x.achieved_at = e.achieved_at AND x.user_id < e.user_id)))`

const tiedExpr = `(SELECT COUNT(*) FROM leaderboard_entries t
 WHERE t.project_id=e.project_id AND t.environment_id=e.environment_id
   AND t.leaderboard_id=e.leaderboard_id AND t.window_key=e.window_key
   AND t.score = e.score)`

// GetLeaderboard ranks a board for a window. `me` (optional user id) adds the
// caller's own row.
func GetLeaderboard(ctx context.Context, pool *db.Pool, projectID, envID, leaderboardID, windowKey, me string, limit int) (*LeaderboardPage, error) {
	if windowKey == "" {
		windowKey = "ever"
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	// Direction comes from the authored object config.
	dir := "highest"
	var cfgRaw json.RawMessage
	if err := pool.QueryRow(ctx, `
		SELECT config FROM objects WHERE id=$1 AND project_id=$2 AND environment_id=$3 AND type='leaderboard'`,
		leaderboardID, projectID, envID).Scan(&cfgRaw); err == nil {
		var cfg map[string]any
		if json.Unmarshal(cfgRaw, &cfg) == nil {
			if d, ok := cfg["direction"].(string); ok && d != "" {
				dir = d
			}
		}
	}
	order := "score DESC, achieved_at ASC, user_id ASC"
	if dir == "lowest" {
		order = "score ASC, achieved_at ASC, user_id ASC"
	}

	page := &LeaderboardPage{LeaderboardID: leaderboardID, WindowKey: windowKey, Direction: dir}

	_ = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM leaderboard_entries
		WHERE project_id=$1 AND environment_id=$2 AND leaderboard_id=$3 AND window_key=$4`,
		projectID, envID, leaderboardID, windowKey).Scan(&page.Total)

	page.Top = queryMaps(ctx, pool, fmt.Sprintf(`
		SELECT user_id, score, to_char(achieved_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS achieved_at,
		       %s AS rank, %s AS tied_with
		FROM leaderboard_entries e
		WHERE e.project_id=$1 AND e.environment_id=$2 AND e.leaderboard_id=$3 AND e.window_key=$4
		ORDER BY %s LIMIT $5`, rankExpr, tiedExpr, order),
		projectID, envID, leaderboardID, windowKey, limit)

	if me != "" {
		rows := queryMaps(ctx, pool, fmt.Sprintf(`
			SELECT user_id, score, to_char(achieved_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS achieved_at,
			       %s AS rank
			FROM leaderboard_entries e
			WHERE e.project_id=$1 AND e.environment_id=$2 AND e.leaderboard_id=$3
			  AND e.window_key=$4 AND e.user_id=$5`, rankExpr),
			projectID, envID, leaderboardID, windowKey, me)
		if len(rows) > 0 {
			page.Me = &rows[0]
		}
	}
	return page, nil
}

// GetTrace fetches the decision trace for one event (§70 "why").
func GetTrace(ctx context.Context, pool *db.Pool, projectID, envID, eventID string) (map[string]any, error) {
	var traceID, actorID string
	var configVersion int
	var nodes json.RawMessage
	var createdAt string
	err := pool.QueryRow(ctx, `
		SELECT trace_id, actor_id, config_version, nodes, to_char(created_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM decision_traces WHERE event_id=$1 AND project_id=$2 AND environment_id=$3`,
		eventID, projectID, envID).
		Scan(&traceID, &actorID, &configVersion, &nodes, &createdAt)
	if err != nil {
		return nil, db.NotFoundError("no trace for this event in this environment")
	}
	var nodeArr []any
	_ = json.Unmarshal(nodes, &nodeArr)
	return map[string]any{
		"trace_id":       traceID,
		"event_id":       eventID,
		"actor_id":       actorID,
		"config_version": configVersion,
		"nodes":          nodeArr,
		"created_at":     createdAt,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Direct economy operations (§27: server-to-server spends/grants with the
// same ledger + NET validation the engine path uses)
// ─────────────────────────────────────────────────────────────────────────────

// EconomyError marks a NET-validation failure.
type EconomyError struct{ Msg string }

func (e *EconomyError) Error() string { return e.Msg }

// AdjustWallet applies a signed ledger delta with idempotency + NET check.
// source identifies the API caller; reference is the caller's idempotency key.
func AdjustWallet(ctx context.Context, pool *db.Pool, projectID, envID, userID, currency string, amount int64, source, reference, reason string, allowNegative bool) (int64, bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (entry_id, project_id, environment_id, wallet_user_id, currency, amount, source, reason, reference)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (project_id, environment_id, source, reference) DO NOTHING`,
		db.NewID("led"), projectID, envID, userID, currency, amount, source, reason, reference)
	if err != nil {
		return 0, false, err
	}
	if tag.RowsAffected() == 0 {
		// Duplicate replay — report the current NET balance.
		var balance int64
		_ = pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount),0) FROM ledger_entries
			WHERE project_id=$1 AND environment_id=$2 AND wallet_user_id=$3 AND currency=$4`,
			projectID, envID, userID, currency).Scan(&balance)
		return balance, false, nil
	}

	var balance int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount),0) FROM ledger_entries
		WHERE project_id=$1 AND environment_id=$2 AND wallet_user_id=$3 AND currency=$4`,
		projectID, envID, userID, currency).Scan(&balance); err != nil {
		return 0, false, err
	}
	if balance < 0 && !allowNegative {
		return 0, false, &EconomyError{Msg: fmt.Sprintf("insufficient `%s` balance: NET %d — refused", currency, balance)}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO wallets (project_id, environment_id, user_id, currency, balance, updated_at)
		VALUES ($1,$2,$3,$4,$5,now())
		ON CONFLICT (project_id, environment_id, user_id, currency)
		DO UPDATE SET balance=$5, updated_at=now()`,
		projectID, envID, userID, currency, balance); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return balance, true, nil
}

// WalletHistory pages the ledger for one wallet.
func WalletHistory(ctx context.Context, pool *db.Pool, projectID, envID, userID, currency string, limit, offset int) ([]map[string]any, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return queryMapsTx(ctx, pool, `
		SELECT jsonb_build_object('entry_id', entry_id, 'amount', amount, 'source', source,
		       'reason', reason, 'reference', reference, 'currency', currency,
		       'occurred_at', to_char(occurred_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
		FROM ledger_entries
		WHERE project_id=$1 AND environment_id=$2 AND wallet_user_id=$3 AND ($4='' OR currency=$4)
		ORDER BY occurred_at DESC LIMIT $5 OFFSET $6`,
		projectID, envID, userID, currency, limit, offset), nil
}

func queryMapsTx(ctx context.Context, pool *db.Pool, q string, args ...any) []map[string]any {
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}
