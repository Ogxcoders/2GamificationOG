// Package middleware: API key authentication, capability authorization,
// tenant scope enforcement (Invariant A), CORS, and request logging.
//
// Security contract (from the platform's §95 + §185/§186):
//   - Authorization: Bearer <keyID.secret> (or X-API-Key header)
//   - The stored hash covers ONLY the secret part.
//   - Every route MUST declare required capabilities (no unauthenticated routes).
//   - Scoped keys (project+environment) are confined to their scope.
package middleware

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"

	"universalengagement/control-plane/internal/db"
)

// Actor is the authenticated caller identity.
type Actor struct {
	KeyID         string
	ProjectID     string // empty = org-wide key
	EnvironmentID string // empty = all environments
	Scopes        []string
}

type ctxKey int

const actorKey ctxKey = 1

// ActorFrom retrieves the actor from the request context (nil if absent).
func ActorFrom(ctx context.Context) *Actor {
	a, _ := ctx.Value(actorKey).(*Actor)
	return a
}

// HasScope reports whether the actor holds an exact capability.
func (a *Actor) HasScope(scope string) bool {
	for _, s := range a.Scopes {
		if s == scope || s == "*" {
			return true
		}
	}
	return false
}

// InScope enforces tenant isolation: org keys are unrestricted, scoped keys
// are confined to their project+environment.
func (a *Actor) InScope(projectID, environmentID string) bool {
	if a.ProjectID == "" {
		return true
	}
	if projectID == "" || a.ProjectID != projectID {
		return false
	}
	if a.EnvironmentID != "" && environmentID != "" && a.EnvironmentID != environmentID {
		return false
	}
	return true
}

// hashSecret hashes ONLY the secret component of a key.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Resolve validates the presented key and returns the actor.
// Returns (nil, nil) when no credentials are presented (route decides 401),
// and an error for malformed credentials.
func Resolve(pool *db.Pool, r *http.Request) (*Actor, error) {
	raw := ""
	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(authz, "Bearer ") {
		raw = strings.TrimPrefix(authz, "Bearer ")
	}
	if raw == "" {
		raw = r.Header.Get("X-API-Key")
	}
	if raw == "" {
		return nil, nil
	}

	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return nil, ErrMalformedKey
	}
	keyID, secret := parts[0], parts[1]

	var (
		projectID string
		envID     string
		keyHash   string
		scopes    []string
		revoked   bool
		revokedAt *string
	)
	err := pool.QueryRow(r.Context(), `
		SELECT k.id, COALESCE(k.project_id::text,''), COALESCE(k.environment_id::text,''),
		       k.key_hash, k.scopes, k.revoked_at IS NOT NULL, k.revoked_at::text
		FROM api_keys k
		WHERE k.id = $1`, keyID).
		Scan(&keyID, &projectID, &envID, &keyHash, &scopes, &revoked, &revokedAt)
	if err != nil {
		return nil, ErrUnknownKey
	}
	if revoked {
		return nil, ErrRevokedKey
	}

	// Constant-time comparison of the hash.
	expect := hashSecret(secret)
	if subtle.ConstantTimeCompare([]byte(expect), []byte(keyHash)) != 1 {
		return nil, ErrBadSecret
	}

	// Record usage (best-effort, non-blocking correctness).
	_, _ = pool.Exec(r.Context(), `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, keyID)

	return &Actor{KeyID: keyID, ProjectID: projectID, EnvironmentID: envID, Scopes: scopes}, nil
}

// Require wraps a handler with authentication + capability enforcement.
// scope == "" means authenticated-only (no specific capability).
func Require(pool *db.Pool, scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := Resolve(pool, r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", err.Error())
			return
		}
		if actor == nil {
			writeError(w, http.StatusUnauthorized, "missing_credentials",
				"this endpoint requires an API key — pass Authorization: Bearer <keyID.secret>")
			return
		}
		if scope != "" && !actor.HasScope(scope) {
			writeError(w, http.StatusForbidden, "missing_capability",
				"this API key lacks the required capability '"+scope+"' — grant it to the key or use a key with broader scopes")
			return
		}
		ctx := context.WithValue(r.Context(), actorKey, actor)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireScope enforces tenant isolation for scoped resources.
// projectID/environmentID come from path variables (extracted by the router).
func RequireScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := ActorFrom(r.Context())
		if actor == nil {
			writeError(w, http.StatusUnauthorized, "missing_credentials", "authentication required")
			return
		}
		projectID := r.PathValue("projectId")
		environmentID := r.PathValue("environmentId")
		if projectID != "" && !actor.InScope(projectID, environmentID) {
			// 404 semantics: scoped keys must not learn other tenants exist.
			writeError(w, http.StatusNotFound, "not_found", "project not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"` + escapeJSON(message) + `"}}`))
}

func escapeJSON(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", " ", "\t", " ")
	return r.Replace(s)
}

// Sentinel auth errors.
var (
	ErrMalformedKey = &AuthError{"malformed_key", "API key must have the form keyID.secret"}
	ErrUnknownKey   = &AuthError{"unknown_key", "the key ID does not exist in this platform"}
	ErrRevokedKey   = &AuthError{"revoked_key", "this API key has been revoked"}
	ErrBadSecret    = &AuthError{"bad_secret", "the API key secret is incorrect"}
)

// AuthError is a typed auth failure.
type AuthError struct {
	Code    string
	Message string
}

func (e *AuthError) Error() string { return e.Message }
