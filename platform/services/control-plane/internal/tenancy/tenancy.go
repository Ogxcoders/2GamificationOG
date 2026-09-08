// Package tenancy: organizations, workspaces, projects, environments, API keys.
package tenancy

import (
        "context"
        "errors"
        "time"

        "universalengagement/control-plane/internal/db"

        "github.com/jackc/pgx/v5"
)

// Capability catalog. The default read-only set deliberately contains NO
// write capabilities (the read-only leak lesson).
var DefaultReadScopes = []string{
        "project.read", "objects.read", "events.read", "traces.read",
        "users.read", "analytics.read", "health.read",
}

var FullScopes = []string{"*"}

// ErrNotFound is the canonical not-found error.
var ErrNotFound = errors.New("not found")

// Organization row.
type Organization struct {
        ID        string    `json:"id"`
        Name      string    `json:"name"`
        Slug      string    `json:"slug"`
        Plan      string    `json:"plan"`
        CreatedAt time.Time `json:"created_at"`
}

// CreateOrganization inserts an org with a unique slug.
func CreateOrganization(ctx context.Context, pool *db.Pool, name, slug string) (*Organization, error) {
        if name == "" {
                return nil, db.ValidationError("name", "organization name is required")
        }
        if slug == "" {
                return nil, db.ValidationError("slug", "organization slug is required")
        }
        org := &Organization{ID: db.NewID("org"), Name: name, Slug: slug}
        err := pool.QueryRow(ctx, `
                INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)
                RETURNING created_at`, org.ID, org.Name, org.Slug).
                Scan(&org.CreatedAt)
        if err != nil {
                if db.IsUniqueViolation(err) {
                        return nil, db.ConflictError("an organization with slug '" + slug + "' already exists")
                }
                return nil, err
        }
        return org, nil
}

// Project row.
type Project struct {
        ID        string    `json:"id"`
        OrgID     string    `json:"org_id"`
        Name      string    `json:"name"`
        Timezone  string    `json:"timezone"`
        CreatedAt time.Time `json:"created_at"`
}

// Environment row.
type Environment struct {
        ID        string `json:"id"`
        ProjectID string `json:"project_id"`
        Kind      string `json:"kind"`
}

// BootstrapTenant creates org + project + 3 environments + an admin key
// (Customer Zero onboarding, §156 steps 1-4).
func BootstrapTenant(ctx context.Context, pool *db.Pool, orgName, projectName string) (*Organization, *Project, []*Environment, *APIKey, error) {
        tx, err := pool.Begin(ctx)
        if err != nil {
                return nil, nil, nil, nil, err
        }
        defer func() { _ = tx.Rollback(ctx) }()

        slug := db.Slugify(orgName)
        org := &Organization{ID: db.NewID("org"), Name: orgName, Slug: slug}
        if err := tx.QueryRow(ctx,
                `INSERT INTO organizations (id, name, slug) VALUES ($1,$2,$3) RETURNING created_at`,
                org.ID, org.Name, org.Slug).Scan(&org.CreatedAt); err != nil {
                if db.IsUniqueViolation(err) {
                        return nil, nil, nil, nil, db.ConflictError("organization slug already in use")
                }
                return nil, nil, nil, nil, err
        }

        project := &Project{ID: db.NewID("proj"), OrgID: org.ID, Name: projectName, Timezone: "UTC"}
        if err := tx.QueryRow(ctx,
                `INSERT INTO projects (id, org_id, name) VALUES ($1,$2,$3) RETURNING created_at`,
                project.ID, project.OrgID, project.Name).Scan(&project.CreatedAt); err != nil {
                return nil, nil, nil, nil, err
        }

        var envs []*Environment
        for _, kind := range []string{"development", "staging", "production"} {
                // Environment ids ARE their kind per project: API paths address
                // environments by kind (…/environments/development) and the UNIQUE
                // (project_id, kind) guarantees one row per kind.
                env := &Environment{ID: kind, ProjectID: project.ID, Kind: kind}
                if _, err := tx.Exec(ctx,
                        `INSERT INTO environments (id, project_id, kind, name) VALUES ($1,$2,$3,$4)`,
                        env.ID, env.ProjectID, env.Kind, env.Kind); err != nil {
                        return nil, nil, nil, nil, err
                }
                envs = append(envs, env)
        }

        key, err := CreateKeyTx(ctx, tx, project.ID, "", "admin", FullScopes)
        if err != nil {
                return nil, nil, nil, nil, err
        }

        if err := tx.Commit(ctx); err != nil {
                return nil, nil, nil, nil, err
        }
        return org, project, envs, key, nil
}

// GetProject fetches one project.
func GetProject(ctx context.Context, pool *db.Pool, id string) (*Project, error) {
        p := &Project{}
        err := pool.QueryRow(ctx, `
                SELECT id, org_id, name, timezone, created_at FROM projects
                WHERE id = $1 AND archived_at IS NULL`, id).
                Scan(&p.ID, &p.OrgID, &p.Name, &p.Timezone, &p.CreatedAt)
        if err != nil {
                return nil, db.NotFoundError("project not found")
        }
        return p, nil
}

// ListProjects returns all projects of an organization.
func ListProjects(ctx context.Context, pool *db.Pool, orgID string) ([]*Project, error) {
        rows, err := pool.Query(ctx, `
                SELECT id, org_id, name, timezone, created_at FROM projects
                WHERE org_id = $1 AND archived_at IS NULL ORDER BY created_at`, orgID)
        if err != nil {
                return nil, err
        }
        defer rows.Close()
        var out []*Project
        for rows.Next() {
                p := &Project{}
                if err := rows.Scan(&p.ID, &p.OrgID, &p.Name, &p.Timezone, &p.CreatedAt); err != nil {
                        return nil, err
                }
                out = append(out, p)
        }
        return out, rows.Err()
}

// GetEnvironmentByKind resolves "development"/"staging"/"production" to the env row.
func GetEnvironmentByKind(ctx context.Context, pool *db.Pool, projectID, kind string) (*Environment, error) {
        e := &Environment{}
        err := pool.QueryRow(ctx, `
                SELECT id, project_id, kind FROM environments WHERE project_id = $1 AND kind = $2`,
                projectID, kind).Scan(&e.ID, &e.ProjectID, &e.Kind)
        if err != nil {
                return nil, db.NotFoundError("environment '" + kind + "' not found in this project")
        }
        return e, nil
}

// GetEnvironment resolves an environment by UUID or by kind name.
func GetEnvironment(ctx context.Context, pool *db.Pool, projectID, envRef string) (*Environment, error) {
        e := &Environment{}
        err := pool.QueryRow(ctx, `
                SELECT id, project_id, kind FROM environments WHERE project_id = $1 AND id = $2`,
                projectID, envRef).Scan(&e.ID, &e.ProjectID, &e.Kind)
        if err != nil {
                return GetEnvironmentByKind(ctx, pool, projectID, envRef)
        }
        return e, nil
}

// APIKey is the issuance record. Secret is shown EXACTLY ONCE at creation.
type APIKey struct {
        ID        string    `json:"id"`     // key_id (public)
        Secret    string    `json:"secret"` // shown once
        ProjectID string    `json:"project_id"`
        EnvID     string    `json:"environment_id"`
        Name      string    `json:"name"`
        Scopes    []string  `json:"scopes"`
        CreatedAt time.Time `json:"created_at"`
}

// CreatedKey returns the full credential: "keyID.secret".
func (k *APIKey) CreatedKey() string { return k.ID + "." + k.Secret }

// CreateAPIKey issues a new key; the stored hash covers ONLY the secret.
func CreateAPIKey(ctx context.Context, pool *db.Pool, projectID, environmentID, name string, scopes []string) (*APIKey, error) {
        if len(scopes) == 0 {
                return nil, db.ValidationError("scopes", "at least one scope is required — use the read-only defaults or '*' for full access")
        }
        if name == "read-only" {
                scopes = DefaultReadScopes
        }
        tx, err := pool.Begin(ctx)
        if err != nil {
                return nil, err
        }
        defer func() { _ = tx.Rollback(ctx) }()
        key, err := CreateKeyTx(ctx, tx, projectID, environmentID, name, scopes)
        if err != nil {
                return nil, err
        }
        return key, tx.Commit(ctx)
}

// CreateKeyTx issues a key inside an existing transaction.
func CreateKeyTx(ctx context.Context, tx pgx.Tx, projectID, environmentID, name string, scopes []string) (*APIKey, error) {
        keyID := db.NewID("key")
        secret := db.RandomToken(32)
        keyHash := db.HashToken(secret)

        k := &APIKey{
                ID:        keyID,
                Secret:    secret,
                ProjectID: projectID,
                EnvID:     environmentID,
                Name:      name,
                Scopes:    scopes,
        }
        if _, err := tx.Exec(ctx, `
                INSERT INTO api_keys (id, project_id, environment_id, name, scopes, key_hash)
                VALUES ($1, NULLIF($2,''), NULLIF($3,''), $4, $5, $6)`,
                keyID, projectID, environmentID, name, scopes, keyHash); err != nil {
                return nil, err
        }
        return k, nil
}

// RevokeAPIKey soft-revokes a key.
func RevokeAPIKey(ctx context.Context, pool *db.Pool, keyID string) error {
        tag, err := pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, keyID)
        if err != nil {
                return err
        }
        if tag.RowsAffected() == 0 {
                return db.NotFoundError("api key not found or already revoked")
        }
        return nil
}
