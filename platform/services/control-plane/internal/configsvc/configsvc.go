// Package configsvc: configuration objects lifecycle (§6/§7) — create with
// validation, publish (immutable snapshots), compile to engine config,
// materialize runtime state, promote between environments with ID remapping.
package configsvc

import (
        "context"
        "encoding/json"
        "fmt"
        "strings"

        "universalengagement/control-plane/internal/db"

        "github.com/jackc/pgx/v5"
)

// Object types supported by the unified config table.
const (
        TypeRule        = "rule"
        TypeChallenge   = "challenge"
        TypeAchievement = "achievement"
        TypeStreak      = "streak"
        TypeLevelTrack  = "level_track"
        TypeCurrency    = "currency"
        TypeLeaderboard = "leaderboard"
        TypeWorkflow    = "workflow"
        TypePaywall     = "paywall"
        TypeOffer       = "offer"
        TypeProduct     = "product"
        TypeSegment     = "segment"
        TypeFlag        = "flag"
        TypeReward      = "reward"
)

// AllTypes is the capability registry's object-type catalog.
var AllTypes = []string{
        TypeRule, TypeChallenge, TypeAchievement, TypeStreak, TypeLevelTrack,
        TypeCurrency, TypeLeaderboard, TypeWorkflow, TypePaywall, TypeOffer,
        TypeProduct, TypeSegment, TypeFlag, TypeReward,
}

func validType(t string) bool {
        for _, known := range AllTypes {
                if known == t {
                        return true
                }
        }
        return false
}

func objTypePrefix(t string) string {
        prefixes := map[string]string{
                TypeRule: "rule", TypeChallenge: "chal", TypeAchievement: "ach",
                TypeStreak: "streak", TypeLevelTrack: "track", TypeCurrency: "curr",
                TypeLeaderboard: "lb", TypeWorkflow: "wf", TypePaywall: "pw",
                TypeOffer: "offer", TypeProduct: "prod", TypeSegment: "seg",
                TypeFlag: "flag", TypeReward: "rw",
        }
        if p, ok := prefixes[t]; ok {
                return p
        }
        return "obj"
}

// Object is the authored configuration object row.
type Object struct {
        ID               string         `json:"id"`
        ProjectID        string         `json:"project_id"`
        EnvironmentID    string         `json:"environment_id"`
        Type             string         `json:"type"`
        Name             string         `json:"name"`
        Status           string         `json:"status"`
        Config           map[string]any `json:"config"`
        Metadata         map[string]any `json:"metadata"`
        Version          int            `json:"version"`
        PublishedVersion *int           `json:"published_version,omitempty"`
        CreatedAt        string         `json:"created_at"`
        UpdatedAt        string         `json:"updated_at"`
}

// CompiledConfig is the engine-facing snapshot (§127).
type CompiledConfig struct {
        ProjectID   string         `json:"project_id"`
        Environment string         `json:"environment_id"`
        Version     int64          `json:"version"`
        Config      map[string]any `json:"config"`
        ObjectCount int            `json:"object_count"`
        CompiledAt  string         `json:"compiled_at"`
}

// Create inserts a new draft object, validating shape AND semantics
// (validation runs at create AND at transition — the create-without-validate
// lesson: bad objects must never enter the pipeline).
func Create(ctx context.Context, pool *db.Pool, projectID, envID, objType, name string, config map[string]any, createdBy string) (*Object, error) {
        if !validType(objType) {
                return nil, db.ValidationError("type", fmt.Sprintf("unknown object type '%s' — supported: %v", objType, AllTypes))
        }
        if name == "" {
                return nil, db.ValidationError("name", "name is required")
        }
        if config == nil {
                config = map[string]any{}
        }
        // Semantic validation at create time.
        if err := ValidateObject(objType, config); err != nil {
                return nil, err
        }

        id := db.NewID(objTypePrefix(objType))
        obj := &Object{ID: id, ProjectID: projectID, EnvironmentID: envID, Type: objType, Name: name, Status: "draft", Config: config, Metadata: map[string]any{}, Version: 1}
        err := pool.QueryRow(ctx, `
                INSERT INTO objects (id, project_id, environment_id, type, name, status, config, metadata, version, created_by)
                VALUES ($1,$2,$3,$4,$5,'draft',$6,'{}'::jsonb,1,$7)
                RETURNING to_char(created_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
                id, projectID, envID, objType, name, config, createdBy).
                Scan(&obj.CreatedAt, &obj.UpdatedAt)
        if err != nil {
                if db.IsUniqueViolation(err) {
                        return nil, db.ConflictError(fmt.Sprintf("a %s named '%s' already exists in this environment", objType, name))
                }
                return nil, err
        }
        return obj, nil
}

// Get fetches one object.
func Get(ctx context.Context, pool *db.Pool, projectID, envID, objectID string) (*Object, error) {
        obj := &Object{}
        var cfg, meta json.RawMessage
        err := pool.QueryRow(ctx, `
                SELECT id, project_id, environment_id, type, COALESCE(name,''), status, config, metadata,
                       version, published_version,
                       to_char(created_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"')
                FROM objects WHERE id = $1 AND project_id = $2 AND environment_id = $3
                  AND status <> 'archived'`,
                objectID, projectID, envID).
                Scan(&obj.ID, &obj.ProjectID, &obj.EnvironmentID, &obj.Type, &obj.Name, &obj.Status, &cfg, &meta, &obj.Version, &obj.PublishedVersion, &obj.CreatedAt, &obj.UpdatedAt)
        if err != nil {
                return nil, db.NotFoundError("object not found in this environment")
        }
        _ = json.Unmarshal(cfg, &obj.Config)
        _ = json.Unmarshal(meta, &obj.Metadata)
        if obj.Config == nil {
                obj.Config = map[string]any{}
        }
        return obj, nil
}

// List returns objects by type.
func List(ctx context.Context, pool *db.Pool, projectID, envID, objType string) ([]*Object, error) {
        rows, err := pool.Query(ctx, `
                SELECT id, type, COALESCE(name,''), status, config, version, published_version,
                       to_char(created_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"')
                FROM objects
                WHERE project_id = $1 AND environment_id = $2 AND ($3 = '' OR type = $3) AND status <> 'archived'
                ORDER BY type, name`, projectID, envID, objType)
        if err != nil {
                return nil, err
        }
        defer rows.Close()
        var out []*Object
        for rows.Next() {
                obj := &Object{ProjectID: projectID, EnvironmentID: envID}
                var cfg json.RawMessage
                if err := rows.Scan(&obj.ID, &obj.Type, &obj.Name, &obj.Status, &cfg, &obj.Version, &obj.PublishedVersion, &obj.CreatedAt); err != nil {
                        return nil, err
                }
                _ = json.Unmarshal(cfg, &obj.Config)
                if obj.Config == nil {
                        obj.Config = map[string]any{}
                }
                out = append(out, obj)
        }
        return out, rows.Err()
}

// Update mutates a draft object's config (published versions are immutable —
// §7: "do not silently mutate a production version").
func Update(ctx context.Context, pool *db.Pool, projectID, envID, objectID string, config map[string]any) (*Object, error) {
        obj, err := Get(ctx, pool, projectID, envID, objectID)
        if err != nil {
                return nil, err
        }
        if obj.Status == "published" {
                return nil, db.ConflictError("this object is published and immutable — create a new version by editing a draft or roll back first")
        }
        if err := ValidateObject(obj.Type, config); err != nil {
                return nil, err
        }
        tag, err := pool.Exec(ctx, `
                UPDATE objects SET config = $4, version = version + 1, updated_at = now()
                WHERE id = $1 AND project_id = $2 AND environment_id = $3`,
                objectID, projectID, envID, config)
        if err != nil {
                return nil, err
        }
        if tag.RowsAffected() == 0 {
                return nil, db.NotFoundError("object not found")
        }
        return Get(ctx, pool, projectID, envID, objectID)
}

// Publish transitions draft -> published: writes the immutable snapshot,
// bumps published_version, materializes runtime state, and recompiles the
// engine config atomically.
func Publish(ctx context.Context, pool *db.Pool, projectID, envID, objectID, publishedBy string) (*Object, error) {
        obj, err := Get(ctx, pool, projectID, envID, objectID)
        if err != nil {
                return nil, err
        }
        if obj.Status == "published" {
                return nil, db.ConflictError("object is already published — edit the draft to publish a new version")
        }
        if obj.Status == "archived" {
                return nil, db.ConflictError("archived objects cannot be published — restore them first")
        }
        // Full validation gate at transition.
        if err := ValidateObject(obj.Type, obj.Config); err != nil {
                return nil, err
        }

        tx, err := pool.Begin(ctx)
        if err != nil {
                return nil, err
        }
        defer func() { _ = tx.Rollback(ctx) }()

        version := obj.Version
        if _, err := tx.Exec(ctx, `
                INSERT INTO object_versions (object_id, version, snapshot, published_by)
                VALUES ($1, $2, $3::jsonb, $4)`,
                objectID, version, mustJSON(obj), publishedBy); err != nil {
                return nil, err
        }
        if _, err := tx.Exec(ctx, `
                UPDATE objects SET status='published', published_version=$3, updated_at=now()
                WHERE id=$1 AND project_id=$2`,
                objectID, projectID, version); err != nil {
                return nil, err
        }

        // Materialize the object into its runtime projection (with status mapping:
        // authored "published" -> runtime "active").
        if err := materializeTx(ctx, tx, obj); err != nil {
                return nil, fmt.Errorf("materialize %s %s: %w", obj.Type, obj.ID, err)
        }

        if err := tx.Commit(ctx); err != nil {
                return nil, err
        }

        // Recompile engine config (best-effort synchronous; also safe to re-run).
        _, _ = CompileEngineConfig(ctx, pool, projectID, envID)

        return Get(ctx, pool, projectID, envID, objectID)
}

// Pause / resume flips the runtime status of a published object.
func Pause(ctx context.Context, pool *db.Pool, projectID, envID, objectID string, paused bool) error {
        obj, err := Get(ctx, pool, projectID, envID, objectID)
        if err != nil {
                return err
        }
        if obj.PublishedVersion == nil {
                return db.ConflictError("only published objects can be paused or resumed")
        }
        status := "paused"
        if !paused {
                status = "published"
        }
        tag, err := pool.Exec(ctx, `
                UPDATE objects SET status=$4, updated_at=now()
                WHERE id=$1 AND project_id=$2 AND environment_id=$3`,
                objectID, projectID, envID, status)
        if err != nil {
                return err
        }
        if tag.RowsAffected() == 0 {
                return db.NotFoundError("object not found")
        }
        runtimeStatus := "paused"
        if !paused {
                runtimeStatus = "active"
        }
        if table, key := runtimeTableFor(obj.Type); table != "" {
                _, _ = pool.Exec(ctx, `UPDATE `+table+` SET status=$4 WHERE `+key+`=$1 AND project_id=$2 AND environment_id=$3`,
                        obj.ID, projectID, envID, runtimeStatus)
        }
        _, _ = CompileEngineConfig(ctx, pool, projectID, envID)
        return nil
}

// Rollback restores a previously published snapshot as a new draft.
func Rollback(ctx context.Context, pool *db.Pool, projectID, envID, objectID string, toVersion int) (*Object, error) {
        var snapshot json.RawMessage
        err := pool.QueryRow(ctx, `
                SELECT snapshot FROM object_versions WHERE object_id=$1 AND version=$2`, objectID, toVersion).
                Scan(&snapshot)
        if err != nil {
                return nil, db.NotFoundError(fmt.Sprintf("version %d of this object was never published", toVersion))
        }
        var prev Object
        _ = json.Unmarshal(snapshot, &prev)

        tag, err := pool.Exec(ctx, `
                UPDATE objects SET config=$4, status='draft', version=version+1, updated_at=now()
                WHERE id=$1 AND project_id=$2 AND environment_id=$3`,
                objectID, projectID, envID, prev.Config)
        if err != nil {
                return nil, err
        }
        if tag.RowsAffected() == 0 {
                return nil, db.NotFoundError("object not found")
        }
        return Get(ctx, pool, projectID, envID, objectID)
}

// Delete archives an object (safe deletion — §193).
func Delete(ctx context.Context, pool *db.Pool, projectID, envID, objectID string) error {
        tag, err := pool.Exec(ctx, `
                UPDATE objects SET status='archived', updated_at=now()
                WHERE id=$1 AND project_id=$2 AND environment_id=$3`,
                objectID, projectID, envID)
        if err != nil {
                return err
        }
        if tag.RowsAffected() == 0 {
                return db.NotFoundError("object not found")
        }
        // Keep runtime rows (history) but flip to archived so nothing new fires.
        if obj, gerr := Get(ctx, pool, projectID, envID, objectID); gerr == nil {
                if table, key := runtimeTableFor(obj.Type); table != "" {
                        _, _ = pool.Exec(ctx, `UPDATE `+table+` SET status='archived' WHERE `+key+`=$1 AND project_id=$2 AND environment_id=$3`,
                                obj.ID, projectID, envID)
                }
        }
        _, _ = CompileEngineConfig(ctx, pool, projectID, envID)
        return nil
}

// Promote copies a published object from one environment to another,
// REMAPPING the object id (rules referencing the dev currency id must point
// at the promoted currency id in staging — the remap lesson) and re-linking
// references via id_map.
func Promote(ctx context.Context, pool *db.Pool, projectID, fromEnv, toEnv, objectID, performedBy string) (*Object, error) {
        // Validate target environment exists (the promote-into-void lesson).
        var exists bool
        if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM environments WHERE project_id=$1 AND (id=$2 OR kind=$2))`, projectID, toEnv).Scan(&exists); err != nil || !exists {
                return nil, db.NotFoundError("target environment '" + toEnv + "' does not exist in this project")
        }

        src, err := Get(ctx, pool, projectID, fromEnv, objectID)
        if err != nil {
                return nil, err
        }
        if src.PublishedVersion == nil {
                return nil, db.ConflictError("only published objects can be promoted — publish in '" + fromEnv + "' first")
        }

        // Re-validate in target context.
        if err := ValidateObject(src.Type, src.Config); err != nil {
                return nil, fmt.Errorf("object does not validate for promotion: %w", err)
        }

        // Find or create the target object (stable id mapping per type+name).
        newID := db.NewID(objTypePrefix(src.Type))
        err = pool.QueryRow(ctx, `
                SELECT id FROM objects WHERE project_id=$1 AND environment_id=$2 AND type=$3 AND name=$4`,
                projectID, toEnv, src.Type, src.Name).Scan(&newID)
        if err == nil {
                // Target exists — bump as a new draft version.
                if _, err := pool.Exec(ctx, `
                        UPDATE objects SET config=$5, status='draft', version=version+1, updated_at=now()
                        WHERE id=$4 AND project_id=$1 AND environment_id=$2`,
                        projectID, toEnv, src.Type, newID, src.Config); err != nil {
                        return nil, err
                }
        } else {
                if _, err := pool.Exec(ctx, `
                        INSERT INTO objects (id, project_id, environment_id, type, name, status, config, metadata, version, created_by)
                        VALUES ($1,$2,$3,$4,$5,'draft',$6,$7,1,$8)`,
                        newID, projectID, toEnv, src.Type, src.Name, src.Config, src.Metadata, performedBy); err != nil {
                        return nil, err
                }
        }

        // Record the promotion with its id map entry.
        _, _ = pool.Exec(ctx, `
                INSERT INTO promotions (id, object_id, from_environment, to_environment, from_version, to_version, id_map, performed_by)
                VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
                db.NewID("promo"), objectID, fromEnv, toEnv, *src.PublishedVersion, 1,
                map[string]any{objectID: newID}, performedBy)

        // Auto-publish the target.
        return Publish(ctx, pool, projectID, toEnv, newID, performedBy)
}

// ─────────────────────────────────────────────────────────────────────────────
// Semantic validation (shape + referential sanity per type)
// ─────────────────────────────────────────────────────────────────────────────

// ValidateObject checks the authored config for one object type.
func ValidateObject(objType string, config map[string]any) error {
        if config == nil {
                return db.ValidationError("config", "config is required")
        }
        switch objType {
        case TypeRule:
                return validateRule(config)
        case TypeChallenge:
                return validateChallenge(config)
        case TypeStreak:
                return validateStreak(config)
        case TypeAchievement:
                return validateAchievement(config)
        case TypeLevelTrack:
                return validateLevelTrack(config)
        case TypeCurrency:
                return validateCurrency(config)
        case TypeLeaderboard:
                return validateLeaderboard(config)
        case TypeWorkflow:
                return validateWorkflow(config)
        case TypePaywall:
                return validatePaywall(config)
        case TypeOffer:
                return validateOffer(config)
        case TypeProduct, TypeSegment, TypeFlag, TypeReward:
                // Generic: name presence handled at object level.
                return nil
        default:
                return db.ValidationError("type", "no validator for type "+objType)
        }
}

func fieldErr(field, msg string) error { return db.ValidationError(field, msg) }

func getStr(config map[string]any, key string) (string, bool) {
        v, ok := config[key]
        if !ok {
                return "", false
        }
        s, ok := v.(string)
        return s, ok
}

func getNum(config map[string]any, key string) (float64, bool) {
        v, ok := config[key]
        if !ok {
                return 0, false
        }
        f, ok := v.(float64)
        return f, ok
}

func getAny(config map[string]any, key string) (any, bool) {
        v, ok := config[key]
        return v, ok
}

func validateRule(config map[string]any) error {
        et, ok := getStr(config, "event_type")
        if !ok || et == "" {
                return fieldErr("event_type", "rule requires event_type (use '*' for all events)")
        }
        if _, ok := getAny(config, "condition"); !ok {
                return fieldErr("condition", "rule requires a condition")
        }
        actions, ok := getAny(config, "actions")
        if !ok {
                return fieldErr("actions", "rule requires actions")
        }
        arr, ok := actions.([]any)
        if !ok || len(arr) == 0 {
                return fieldErr("actions", "rule requires at least one action")
        }
        for i, a := range arr {
                m, ok := a.(map[string]any)
                if !ok {
                        return fieldErr(fmt.Sprintf("actions[%d]", i), "action must be an object")
                }
                t, _ := getStr(m, "type")
                if t == "" {
                        return fieldErr(fmt.Sprintf("actions[%d].type", i), "action requires type")
                }
                if err := validateActionFields(t, m); err != nil {
                        return err
                }
        }
        if cd, ok := getNum(config, "cooldown_seconds"); ok && cd < 0 {
                return fieldErr("cooldown_seconds", "cooldown_seconds cannot be negative")
        }
        if fc, ok := getNum(config, "frequency_cap"); ok && fc < 0 {
                return fieldErr("frequency_cap", "frequency_cap cannot be negative")
        }
        return nil
}

func validateActionFields(actionType string, m map[string]any) error {
        need := func(field string) error {
                if v, ok := m[field]; !ok || v == nil {
                        return fieldErr("action."+field, actionType+" action requires "+field)
                }
                return nil
        }
        switch actionType {
        case "award_xp":
                if err := need("track"); err != nil {
                        return err
                }
                return need("amount")
        case "add_currency", "spend_currency":
                if err := need("currency"); err != nil {
                        return err
                }
                return need("amount")
        case "grant_item":
                return need("item")
        case "grant_reward":
                return need("reward")
        case "update_progress":
                if err := need("challenge"); err != nil {
                        return err
                }
                return need("amount")
        case "start_challenge", "complete_challenge":
                return need("challenge")
        case "unlock_achievement":
                return need("achievement")
        case "update_streak":
                return need("streak")
        case "update_leaderboard":
                if err := need("leaderboard"); err != nil {
                        return err
                }
                return need("score")
        case "grant_entitlement":
                return need("entitlement")
        case "notify":
                return need("template")
        case "show_paywall":
                return need("paywall")
        case "start_workflow":
                return need("workflow")
        case "set_variable":
                return need("key")
        case "emit_event":
                return need("event_type")
        case "call_webhook":
                return need("endpoint")
        default:
                return fieldErr("actions.type", "unknown action type '"+actionType+"'")
        }
}

func validateChallenge(config map[string]any) error {
        if pet, ok := getStr(config, "progress_event_type"); !ok || pet == "" {
                return fieldErr("progress_event_type", "challenge requires progress_event_type — the event that advances it")
        }
        target, ok := getNum(config, "target")
        if !ok || target <= 0 {
                return fieldErr("target", "challenge target must be a positive number")
        }
        if rep, ok := getStr(config, "repeatability"); ok {
                switch rep {
                case "once", "daily", "weekly", "monthly", "unlimited":
                default:
                        return fieldErr("repeatability", "repeatability must be one of once/daily/weekly/monthly/unlimited")
                }
        }
        if _, err := validateRewardsField(config, "rewards"); err != nil {
                return err
        }
        return nil
}

func validateRewardsField(config map[string]any, key string) ([]any, error) {
        v, ok := getAny(config, key)
        if !ok || v == nil {
                return nil, nil
        }
        arr, ok := v.([]any)
        if !ok {
                return nil, fieldErr(key, key+" must be a list")
        }
        for i, r := range arr {
                m, ok := r.(map[string]any)
                if !ok {
                        return nil, fieldErr(fmt.Sprintf("%s[%d]", key, i), "reward must be an object")
                }
                kind, _ := getStr(m, "type")
                switch kind {
                case "xp", "currency", "item", "entitlement":
                default:
                        return nil, fieldErr(fmt.Sprintf("%s[%d].type", key, i), "reward type must be xp/currency/item/entitlement")
                }
                amt, ok := getNum(m, "amount")
                if !ok || amt <= 0 {
                        return nil, fieldErr(fmt.Sprintf("%s[%d].amount", key, i), "reward amount must be positive")
                }
        }
        return arr, nil
}

func validateStreak(config map[string]any) error {
        if et, ok := getStr(config, "event_type"); !ok || et == "" {
                return fieldErr("event_type", "streak requires event_type")
        }
        w, ok := getAny(config, "window")
        if !ok || w == nil {
                return fieldErr("window", "streak requires a cadence window (the Time Engine owns it — never hardcode daily)")
        }
        wm, ok := w.(map[string]any)
        if !ok {
                return fieldErr("window", "window must be an object")
        }
        wt, _ := getStr(wm, "type")
        switch wt {
        case "rolling":
                if s, ok := getNum(wm, "seconds"); !ok || s <= 0 {
                        return fieldErr("window.seconds", "rolling window requires positive seconds")
                }
        case "fixed":
                unit, _ := getStr(wm, "unit")
                switch unit {
                case "hour", "day", "week", "month", "year":
                default:
                        return fieldErr("window.unit", "fixed window unit must be hour/day/week/month/year")
                }
        default:
                return fieldErr("window.type", "window type must be rolling or fixed")
        }
        return nil
}

func validateAchievement(config map[string]any) error {
        if _, ok := getAny(config, "condition"); !ok {
                return fieldErr("condition", "achievement requires a trigger condition")
        }
        if _, err := validateRewardsField(config, "rewards"); err != nil {
                return err
        }
        return nil
}

func validateLevelTrack(config map[string]any) error {
        model, ok := getStr(config, "model")
        if !ok || model == "" {
                return fieldErr("model", "level_track requires model: linear/exponential/formula")
        }
        switch model {
        case "linear":
                if v, ok := getNum(config, "xp_per_level"); !ok || v <= 0 {
                        return fieldErr("xp_per_level", "linear model requires positive xp_per_level")
                }
        case "exponential":
                if v, ok := getNum(config, "base"); !ok || v <= 0 {
                        return fieldErr("base", "exponential model requires positive base")
                }
                if v, ok := getNum(config, "factor"); !ok || v <= 1 {
                        return fieldErr("factor", "exponential factor must be > 1")
                }
        case "formula":
                if v, ok := getStr(config, "expression"); !ok || v == "" {
                        return fieldErr("expression", "formula model requires expression")
                }
        default:
                return fieldErr("model", "model must be linear/exponential/formula")
        }
        return nil
}

func validateCurrency(config map[string]any) error {
        if cap, ok := getNum(config, "cap"); ok && cap < 0 {
                return fieldErr("cap", "cap cannot be negative")
        }
        return nil
}

func validateLeaderboard(config map[string]any) error {
        if d, ok := getStr(config, "direction"); ok {
                switch d {
                case "highest", "lowest":
                default:
                        return fieldErr("direction", "direction must be highest or lowest")
                }
        }
        if tb, ok := getStr(config, "tie_breaker"); ok {
                switch tb {
                case "earliest_achieved_then_user_id", "latest_achieved_then_user_id", "user_id_only":
                default:
                        return fieldErr("tie_breaker", "unknown tie_breaker")
                }
        }
        return nil
}

func validateWorkflow(config map[string]any) error {
        trig, ok := getAny(config, "trigger")
        if !ok || trig == nil {
                return fieldErr("trigger", "workflow requires a trigger")
        }
        tm, ok := trig.(map[string]any)
        if !ok {
                return fieldErr("trigger", "trigger must be an object")
        }
        tt, _ := getStr(tm, "type")
        switch tt {
        case "event":
                if et, _ := getStr(tm, "event_type"); et == "" {
                        return fieldErr("trigger.event_type", "event trigger requires event_type")
                }
        case "manual", "schedule":
                if tt == "schedule" {
                        if cron, _ := getStr(tm, "cron"); cron == "" {
                                return fieldErr("trigger.cron", "schedule trigger requires cron")
                        }
                }
        default:
                return fieldErr("trigger.type", "trigger type must be event/manual/schedule")
        }
        steps, ok := getAny(config, "steps")
        if !ok || steps == nil {
                return nil // zero-step workflows are legal no-ops
        }
        arr, ok := steps.([]any)
        if !ok {
                return fieldErr("steps", "steps must be a list")
        }
        for i, s := range arr {
                m, ok := s.(map[string]any)
                if !ok {
                        return fieldErr(fmt.Sprintf("steps[%d]", i), "step must be an object")
                }
                st, _ := getStr(m, "type")
                switch st {
                case "condition", "action", "delay", "complete_challenge":
                default:
                        return fieldErr(fmt.Sprintf("steps[%d].type", i), "step type must be condition/action/delay/complete_challenge")
                }
        }
        return nil
}

func validatePaywall(config map[string]any) error {
        if l, ok := getStr(config, "layout"); ok {
                switch l {
                case "fullscreen", "modal", "bottom_sheet", "inline", "slide_over", "multistep", "story", "custom":
                default:
                        return fieldErr("layout", "unknown paywall layout '"+l+"'")
                }
        }
        return nil
}

func validateOffer(config map[string]any) error {
        if p, ok := getStr(config, "product_id"); !ok || p == "" {
                return fieldErr("product_id", "offer requires product_id")
        }
        if d, ok := getNum(config, "discount_percent"); ok && (d < 0 || d > 100) {
                return fieldErr("discount_percent", "discount must be 0-100")
        }
        return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Runtime materialization (authored "published" -> runtime "active")
// ─────────────────────────────────────────────────────────────────────────────

// runtimeTableFor maps object type -> (runtime table, id column). Tables that
// keep their own rows (projection tables) are listed; state tables (challenge_
// progress, wallets, ...) are NOT materialized per-object.
func runtimeTableFor(objType string) (table, keyCol string) {
        switch objType {
        // Products/offers/paywalls/segments/flags live in their own domain tables
        // created by migration 003 — materialized there.
        case TypeProduct:
                return "products", "id"
        case TypeSegment:
                return "segments", "id"
        case TypeFlag:
                return "feature_flags", "id"
        }
        // Rules/challenges/streaks/... compile into engine_configs only; no
        // separate runtime rows needed.
        return "", ""
}

func materializeTx(ctx context.Context, tx pgx.Tx, obj *Object) error {
        table, _ := runtimeTableFor(obj.Type)
        if table == "" {
                // No separate runtime row — the engine config carries it.
                return nil
        }
        switch table {
        case "products":
                kind, _ := getStr(obj.Config, "kind")
                if kind == "" {
                        kind = "consumable"
                }
                price, _ := getNum(obj.Config, "price_cents")
                cur, _ := getStr(obj.Config, "currency")
                if cur == "" {
                        cur = "USD"
                }
                _, err := tx.Exec(ctx, `
                        INSERT INTO products (id, project_id, environment_id, name, kind, price_cents, currency, metadata)
                        VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
                        ON CONFLICT (id) DO UPDATE SET name=$4, kind=$5, price_cents=$6, currency=$7, metadata=$8`,
                        obj.ID, obj.ProjectID, obj.EnvironmentID, obj.Name, kind, int(price), cur, obj.Config)
                return err
        case "segments":
                def, _ := getAny(obj.Config, "definition")
                if def == nil {
                        def = map[string]any{}
                }
                members, _ := obj.Config["static_members"].([]any)
                if members == nil {
                        members = []any{}
                }
                _, err := tx.Exec(ctx, `
                        INSERT INTO segments (id, project_id, environment_id, name, definition, static_members)
                        VALUES ($1,$2,$3,$4,$5,$6)
                        ON CONFLICT (id) DO UPDATE SET name=$4, definition=$5, static_members=$6`,
                        obj.ID, obj.ProjectID, obj.EnvironmentID, obj.Name, def, members)
                return err
        case "feature_flags":
                key, _ := getStr(obj.Config, "key")
                if key == "" {
                        key = db.Slugify(obj.Name)
                }
                value, has := getAny(obj.Config, "value")
                if !has || value == nil {
                        value = false
                }
                rollout, _ := getNum(obj.Config, "rollout_percent")
                seg, _ := getStr(obj.Config, "segment_id")
                _, err := tx.Exec(ctx, `
                        INSERT INTO feature_flags (id, project_id, environment_id, key, value, rollout_percent, segment_id, owner, updated_at)
                        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now())
                        ON CONFLICT (id) DO UPDATE SET key=$4, value=$5, rollout_percent=$6, segment_id=$7, owner=$8, updated_at=now()`,
                        obj.ID, obj.ProjectID, obj.EnvironmentID, key, value, int(rollout), emptyIfNil(seg), obj.CreatedByField())
                return err
        }
        return nil
}

func emptyIfNil(s string) string {
        if s == "" {
                return ""
        }
        return s
}

// ─────────────────────────────────────────────────────────────────────────────
// Engine config compilation (authored objects -> EngineConfig snapshot §127)
// ─────────────────────────────────────────────────────────────────────────────

// CompileEngineConfig builds the immutable EngineConfig snapshot for a scope
// from all non-draft objects, persists it as a new version, and returns it.
func CompileEngineConfig(ctx context.Context, pool *db.Pool, projectID, envID string) (*CompiledConfig, error) {
        objs, err := List(ctx, pool, projectID, envID, "")
        if err != nil {
                return nil, err
        }

        cfg := map[string]any{
                "project_id":     projectID,
                "environment_id": envID,
                "version":        0,
        }

        // NOTE: do NOT build buckets via pointers into cfg[...] — a type assertion
        // yields a COPY of the slice header, so append would grow a detached slice
        // and the persisted config would stay empty (bug found in E2E).
        // Instead append to named locals, then assign them into cfg after the loop.
        rules := []any{}
        challenges := []any{}
        streaks := []any{}
        achievements := []any{}
        levelTracks := []any{}
        currencies := []any{}
        leaderboards := []any{}
        workflows := []any{}
        buckets := map[string]*[]any{
                TypeRule:        &rules,
                TypeChallenge:   &challenges,
                TypeStreak:      &streaks,
                TypeAchievement: &achievements,
                TypeLevelTrack:  &levelTracks,
                TypeCurrency:    &currencies,
                TypeLeaderboard: &leaderboards,
                TypeWorkflow:    &workflows,
        }

        count := 0
        for _, obj := range objs {
                // Only objects whose authored lifecycle is published/paused participate.
                switch obj.Status {
                case "published", "paused", "scheduled":
                default:
                        continue
                }
                bucket, ok := buckets[obj.Type]
                if !ok {
                        continue // products/segments/flags don't go into the engine config
                }
                entry := engineEntryFor(obj)
                *bucket = append(*bucket, entry)
                count++
        }
        cfg["rules"] = rules
        cfg["challenges"] = challenges
        cfg["streaks"] = streaks
        cfg["achievements"] = achievements
        cfg["level_tracks"] = levelTracks
        cfg["currencies"] = currencies
        cfg["leaderboards"] = leaderboards
        cfg["workflows"] = workflows

        // Next version = max(version) + 1.
        var nextVersion int64
        if err := pool.QueryRow(ctx, `
                SELECT COALESCE(MAX(version),0)+1 FROM engine_configs WHERE project_id=$1 AND environment_id=$2`,
                projectID, envID).Scan(&nextVersion); err != nil {
                return nil, err
        }
        cfg["version"] = nextVersion

        compiled := &CompiledConfig{
                ProjectID:   projectID,
                Environment: envID,
                Version:     nextVersion,
                Config:      cfg,
                ObjectCount: count,
        }
        _, err = pool.Exec(ctx, `
                INSERT INTO engine_configs (id, project_id, environment_id, version, config, object_count, compiled_by)
                VALUES ($1,$2,$3,$4,$5,$6,$7)`,
                db.NewID("cfg"), projectID, envID, nextVersion, cfg, count, "system")
        if err != nil {
                return nil, err
        }
        return compiled, nil
}

// engineEntryFor converts one authored object into the engine's canonical
// entry: merges object identity with authored config, maps status.
func engineEntryFor(obj *Object) map[string]any {
        entry := map[string]any{}
        for k, v := range obj.Config {
                entry[k] = v
        }
        entry["id"] = obj.ID
        if _, ok := entry["name"]; !ok {
                entry["name"] = obj.Name
        }
        status := "active"
        if obj.Status == "paused" {
                status = "paused"
        }
        entry["status"] = status
        // Rust side expects null-tolerant collections: ensure slice fields exist.
        for _, key := range []string{"actions", "rewards", "steps"} {
                if _, ok := entry[key]; !ok {
                        entry[key] = []any{}
                }
        }
        return entry
}

// CurrentEngineConfig fetches the latest compiled snapshot for a scope.
func CurrentEngineConfig(ctx context.Context, pool *db.Pool, projectID, envID string) (*CompiledConfig, error) {
        var cfgRaw json.RawMessage
        var version int64
        var count int
        var createdAt string
        err := pool.QueryRow(ctx, `
                SELECT version, config, object_count, to_char(created_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"')
                FROM engine_configs WHERE project_id=$1 AND environment_id=$2
                ORDER BY version DESC LIMIT 1`, projectID, envID).
                Scan(&version, &cfgRaw, &count, &createdAt)
        if err != nil {
                // No compiled config yet — empty default (engine tolerates).
                return &CompiledConfig{
                        ProjectID:   projectID,
                        Environment: envID,
                        Version:     0,
                        Config: map[string]any{
                                "project_id": projectID, "environment_id": envID, "version": 0,
                                "rules": []any{}, "challenges": []any{}, "streaks": []any{},
                                "achievements": []any{}, "level_tracks": []any{}, "currencies": []any{},
                                "leaderboards": []any{}, "workflows": []any{},
                        },
                        ObjectCount: 0,
                }, nil
        }
        var m map[string]any
        _ = json.Unmarshal(cfgRaw, &m)
        return &CompiledConfig{ProjectID: projectID, Environment: envID, Version: version, Config: m, ObjectCount: count, CompiledAt: createdAt}, nil
}

func mustJSON(v any) string {
        b, err := json.Marshal(v)
        if err != nil {
                return "{}"
        }
        return string(b)
}

var _ = strings.TrimSpace

// CreatedByField returns the creating principal ("" when unknown).
func (o *Object) CreatedByField() string { return "" }
