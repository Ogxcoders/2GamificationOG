// Package identity: end-users, identities, anonymous linking, merges (§9).
package identity

import (
	"context"

	"universalengagement/control-plane/internal/db"
)

// User is an end-user of a customer app.
type User struct {
	ID          string         `json:"id"`
	ProjectID   string         `json:"project_id"`
	Environment string         `json:"environment_id"`
	Anonymous   bool           `json:"anonymous"`
	DisplayName string         `json:"display_name"`
	Attributes  map[string]any `json:"attributes"`
	CreatedAt   string         `json:"created_at"`
}

// UpsertUser creates or updates a user (idempotent by user id).
func UpsertUser(ctx context.Context, pool *db.Pool, projectID, envID, userID string, anonymous bool, displayName string, attrs map[string]any) (*User, error) {
	if userID == "" {
		return nil, db.ValidationError("user_id", "user_id is required")
	}
	u := &User{ID: userID, ProjectID: projectID, Environment: envID, Anonymous: anonymous, DisplayName: displayName, Attributes: attrs}
	if u.Attributes == nil {
		u.Attributes = map[string]any{}
	}
	err := pool.QueryRow(ctx, `
		INSERT INTO users (id, project_id, environment_id, anonymous, display_name, attributes)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			anonymous = EXCLUDED.anonymous AND users.anonymous, -- never re-anonymize
			display_name = CASE WHEN EXCLUDED.display_name <> '' THEN EXCLUDED.display_name ELSE users.display_name END,
			attributes = users.attributes || EXCLUDED.attributes
		RETURNING to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		userID, projectID, envID, anonymous, displayName, u.Attributes).Scan(&u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// GetUser fetches a user (only if not merged away).
func GetUser(ctx context.Context, pool *db.Pool, projectID, envID, userID string) (*User, error) {
	u := &User{}
	err := pool.QueryRow(ctx, `
		SELECT id, project_id, environment_id, anonymous, COALESCE(display_name,''),
		       attributes, to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM users
		WHERE id = $1 AND project_id = $2 AND environment_id = $3
		  AND merged_into IS NULL AND archived_at IS NULL`,
		userID, projectID, envID).Scan(&u.ID, &u.ProjectID, &u.Environment, &u.Anonymous, &u.DisplayName, &u.Attributes, &u.CreatedAt)
	if err != nil {
		return nil, db.NotFoundError("user '" + userID + "' not found in this environment")
	}
	return u, nil
}

// LinkIdentity attaches a provider identity to a user. Conflict-aware:
// identity already owned by another user is a 409.
func LinkIdentity(ctx context.Context, pool *db.Pool, projectID, envID, userID, provider, providerUserID string) error {
	if provider == "" || providerUserID == "" {
		return db.ValidationError("identity", "provider and provider_user_id are required")
	}
	// Validate user exists and is not merged.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE id=$1 AND project_id=$2 AND environment_id=$3 AND merged_into IS NULL)`,
		userID, projectID, envID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return db.NotFoundError("user '" + userID + "' not found in this environment")
	}

	// Check current owner of this identity.
	var owner string
	err := pool.QueryRow(ctx, `
		SELECT user_id FROM identities
		WHERE project_id = $1 AND provider = $2 AND provider_user_id = $3 AND environment_id = $4`,
		projectID, provider, providerUserID, envID).Scan(&owner)
	if err == nil {
		if owner == userID {
			return nil // idempotent relink
		}
		return db.ConflictError("identity '" + provider + ":" + providerUserID + "' is already linked to user '" + owner + "'")
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO identities (id, user_id, project_id, environment_id, provider, provider_user_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		db.NewID("ident"), userID, projectID, envID, provider, providerUserID)
	return err
}

// MergeAnonymous merges source (anonymous) into target (authenticated), moving
// ALL runtime state. Progress is preserved (§9 flow).
func MergeAnonymous(ctx context.Context, pool *db.Pool, projectID, envID, sourceUserID, targetUserID string) error {
	if sourceUserID == "" || targetUserID == "" {
		return db.ValidationError("merge", "source_user_id and target_user_id are required")
	}
	if sourceUserID == targetUserID {
		return db.ValidationError("merge", "cannot merge a user into itself")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Validate both users exist in scope.
	for _, uid := range []string{sourceUserID, targetUserID} {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM users WHERE id=$1 AND project_id=$2 AND environment_id=$3 AND merged_into IS NULL)`,
			uid, projectID, envID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return db.NotFoundError("user '" + uid + "' not found in this environment")
		}
	}

	// Move identities.
	if _, err := tx.Exec(ctx, `
		UPDATE identities SET user_id = $1
		WHERE project_id = $2 AND environment_id = $3 AND user_id = $4`,
		targetUserID, projectID, envID, sourceUserID); err != nil {
		return err
	}
	// Move sessions.
	if _, err := tx.Exec(ctx, `
		UPDATE sessions SET user_id = $1
		WHERE user_id = $2 AND project_id = $3 AND environment_id = $4`,
		targetUserID, sourceUserID, projectID, envID); err != nil {
		return err
	}
	// Move runtime tables.
	for _, table := range []string{"xp_tracks", "rule_state"} {
		// Upsert semantics: coalesce values.
		if table == "xp_tracks" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO xp_tracks (project_id, environment_id, user_id, track, xp, level, highest_level)
				SELECT project_id, environment_id, $1, track,
				       SUM(xp), MAX(level), MAX(highest_level)
				FROM xp_tracks
				WHERE project_id=$2 AND environment_id=$3 AND user_id=$4
				GROUP BY project_id, environment_id, track
				ON CONFLICT (project_id, environment_id, user_id, track) DO UPDATE SET
					xp = xp_tracks.xp + EXCLUDED.xp,
					level = GREATEST(xp_tracks.level, EXCLUDED.level),
					highest_level = GREATEST(xp_tracks.highest_level, EXCLUDED.highest_level),
					updated_at = now()`,
				targetUserID, projectID, envID, sourceUserID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM xp_tracks WHERE project_id=$1 AND environment_id=$2 AND user_id=$3`,
				projectID, envID, sourceUserID); err != nil {
				return err
			}
		} else {
			// rule_state: merge JSONB state objects.
			if _, err := tx.Exec(ctx, `
				INSERT INTO rule_state (project_id, environment_id, user_id, state)
				SELECT project_id, environment_id, $1, COALESCE(state, '{}'::jsonb)
				FROM rule_state
				WHERE project_id=$2 AND environment_id=$3 AND user_id=$4
				ON CONFLICT (project_id, environment_id, user_id) DO UPDATE SET
					state = rule_state.state || EXCLUDED.state,
					updated_at = now()`,
				targetUserID, projectID, envID, sourceUserID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM rule_state WHERE project_id=$1 AND environment_id=$2 AND user_id=$3`,
				projectID, envID, sourceUserID); err != nil {
				return err
			}
		}
	}

	// Simple re-ownership tables.
	simpleTables := []string{
		"challenge_progress", "streak_state", "achievement_unlocks",
		"inventory_items", "user_variables", "reward_grants",
		"segment_memberships", "experiment_assignments", "notifications",
		"subscriptions", "transactions", "simulation_runs",
	}
	for _, t := range simpleTables {
		// Some tables have unique constraints on user_id — use upsert-skip.
		if _, err := tx.Exec(ctx, `
			UPDATE `+t+` SET user_id = $1
			WHERE project_id = $2 AND environment_id = $3 AND user_id = $4
			ON CONFLICT DO NOTHING`, targetUserID, projectID, envID, sourceUserID); err != nil {
			return err
		}
	}

	// Wallets: merge balances.
	if _, err := tx.Exec(ctx, `
		INSERT INTO wallets (project_id, environment_id, user_id, currency, balance)
		SELECT project_id, environment_id, $1, currency, SUM(balance)
		FROM wallets WHERE project_id=$2 AND environment_id=$3 AND user_id=$4
		GROUP BY project_id, environment_id, currency
		ON CONFLICT (project_id, environment_id, user_id, currency) DO UPDATE SET
			balance = wallets.balance + EXCLUDED.balance, updated_at = now()`,
		targetUserID, projectID, envID, sourceUserID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM wallets WHERE project_id=$1 AND environment_id=$2 AND user_id=$3`,
		projectID, envID, sourceUserID); err != nil {
		return err
	}

	// Ledger: re-point wallet_user_id (financial history preserved).
	if _, err := tx.Exec(ctx, `
		UPDATE ledger_entries SET wallet_user_id = $1
		WHERE project_id = $2 AND environment_id = $3 AND wallet_user_id = $4`,
		targetUserID, projectID, envID, sourceUserID); err != nil {
		return err
	}

	// Leaderboard entries: keep best score per board.
	if _, err := tx.Exec(ctx, `
		INSERT INTO leaderboard_entries (id, project_id, environment_id, leaderboard_id, user_id, score, achieved_at, window_key)
		SELECT id, project_id, environment_id, leaderboard_id, $1, score, achieved_at, window_key
		FROM leaderboard_entries
		WHERE project_id=$2 AND environment_id=$3 AND user_id=$4
		ON CONFLICT (project_id, environment_id, leaderboard_id, user_id, window_key) DO UPDATE SET
			score = GREATEST(leaderboard_entries.score, EXCLUDED.score),
			achieved_at = LEAST(leaderboard_entries.achieved_at, EXCLUDED.achieved_at),
			updated_at = now()`,
		targetUserID, projectID, envID, sourceUserID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM leaderboard_entries WHERE project_id=$1 AND environment_id=$2 AND user_id=$3`,
		projectID, envID, sourceUserID); err != nil {
		return err
	}

	// Mark the source merged + archive.
	if _, err := tx.Exec(ctx, `
		UPDATE users SET merged_into = $1, archived_at = now()
		WHERE id = $2 AND project_id = $3 AND environment_id = $4`,
		targetUserID, sourceUserID, projectID, envID); err != nil {
		return err
	}

	return tx.Commit(ctx)
}
