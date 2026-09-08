// Package engine: HTTP client for the Rust engine-service.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client talks to engine-service.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient builds the client.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	}
}

// ProcessRequest is the /v1/process contract.
type ProcessRequest struct {
	Event      map[string]any `json:"event"`
	Config     map[string]any `json:"config,omitempty"`
	ActorState map[string]any `json:"actor_state,omitempty"`
	DryRun     bool           `json:"dry_run"`
}

// ProcessResponse is the /v1/process reply.
type ProcessResponse struct {
	OK        bool           `json:"ok"`
	Error     string         `json:"error,omitempty"`
	ErrorCode string         `json:"error_code,omitempty"`
	Outcome   *EngineOutcome `json:"outcome,omitempty"`
}

// EngineOutcome mirrors the Rust EngineOutcome (snake_case serde names).
type EngineOutcome struct {
	EventID              string            `json:"event_id"`
	Trace                Trace             `json:"trace"`
	Commands             []Command         `json:"commands"`
	RulesMatched         []string          `json:"rules_matched"`
	ChallengesCompleted  []string          `json:"challenges_completed"`
	AchievementsUnlocked []string          `json:"achievements_unlocked"`
	StreakEffects        map[string]string `json:"streak_effects"`
	LevelUps             map[string]int64  `json:"level_ups"`
	LeaderboardUpdates   map[string]int64  `json:"leaderboard_updates"`
	WalletDeltas         map[string]int64  `json:"wallet_deltas"`
	XpDeltas             map[string]int64  `json:"xp_deltas"`
	WorkflowRuns         []map[string]any  `json:"workflow_runs"`
	State                map[string]any    `json:"state"`
	Warnings             []string          `json:"warnings"`
}

// Trace is the decision trace.
type Trace struct {
	TraceID       string      `json:"trace_id"`
	EventID       string      `json:"event_id"`
	CorrelationID string      `json:"correlation_id"`
	ProjectID     string      `json:"project_id"`
	EnvironmentID string      `json:"environment_id"`
	ActorID       string      `json:"actor_id"`
	ConfigVersion int64       `json:"config_version"`
	Nodes         []TraceNode `json:"nodes"`
}

// TraceNode is one trace DAG node.
type TraceNode struct {
	ID     string         `json:"id"`
	Parent string         `json:"parent"`
	Kind   string         `json:"kind"`
	Label  string         `json:"label"`
	Detail map[string]any `json:"detail"`
}

// Command is an engine command to apply.
type Command struct {
	CommandID      string      `json:"command_id"`
	IdempotencyKey string      `json:"idempotency_key"`
	Source         string      `json:"source"`
	EventID        string      `json:"event_id"`
	Kind           CommandKind `json:"kind"`
	Explanation    string      `json:"explanation"`
}

// CommandKind is the type-tagged operation (flattened serde tag).
type CommandKind struct {
	Type         string            `json:"type"`
	Track        string            `json:"track,omitempty"`
	Amount       int64             `json:"amount,omitempty"`
	Currency     string            `json:"currency,omitempty"`
	Item         string            `json:"item,omitempty"`
	Quantity     int64             `json:"quantity,omitempty"`
	Reward       string            `json:"reward,omitempty"`
	Challenge    string            `json:"challenge,omitempty"`
	Achievement  string            `json:"achievement,omitempty"`
	Streak       string            `json:"streak,omitempty"`
	WindowKey    string            `json:"window_key,omitempty"`
	GraceApplied bool              `json:"grace_applied,omitempty"`
	Leaderboard  string            `json:"leaderboard,omitempty"`
	Delta        int64             `json:"delta,omitempty"`
	Score        int64             `json:"score,omitempty"`
	Entitlement  string            `json:"entitlement,omitempty"`
	Duration     int64             `json:"duration_seconds,omitempty"`
	Template     string            `json:"template,omitempty"`
	Params       map[string]string `json:"params,omitempty"`
	Paywall      string            `json:"paywall,omitempty"`
	Workflow     string            `json:"workflow,omitempty"`
	Input        json.RawMessage   `json:"input,omitempty"`
	Key          string            `json:"key,omitempty"`
	Value        json.RawMessage   `json:"value,omitempty"`
	EventType    string            `json:"event_type,omitempty"`
	Payload      map[string]any    `json:"payload,omitempty"`
	Endpoint     string            `json:"endpoint,omitempty"`
}

// Process sends one event for processing.
func (c *Client) Process(ctx context.Context, req *ProcessRequest) (*ProcessResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/process", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	return doRequest[ProcessResponse](c.HTTP, httpReq)
}

// PushConfig caches a compiled engine config inside the engine.
func (c *Client) PushConfig(ctx context.Context, projectID, envID string, config map[string]any) error {
	body, _ := json.Marshal(map[string]any{
		"project_id":     projectID,
		"environment_id": envID,
		"config":         config,
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/config", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	_, err = doRequest[map[string]any](c.HTTP, httpReq)
	return err
}

// CompileCheck validates a config structure.
func (c *Client) CompileCheck(ctx context.Context, config map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(config)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/compile-check", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	res, err := doRequest[map[string]any](c.HTTP, httpReq)
	if err != nil {
		return nil, err
	}
	return *res, nil
}

// Simulate runs a deterministic cohort simulation.
func (c *Client) Simulate(ctx context.Context, req map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/simulate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	res, err := doRequest[map[string]any](c.HTTP, httpReq)
	if err != nil {
		return nil, err
	}
	return *res, nil
}

// Healthz checks the engine.
func (c *Client) Healthz(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	_, err = doRequest[map[string]any](c.HTTP, httpReq)
	return err
}

// doRequest is a package-level generic (Go methods cannot take type params).
func doRequest[T any](client *http.Client, req *http.Request) (*T, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("engine unreachable at %s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var errBody map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		msg := fmt.Sprintf("engine returned %d", resp.StatusCode)
		if m, ok := errBody["error"].(string); ok && m != "" {
			msg = m
		}
		return nil, fmt.Errorf("%s", msg)
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode engine response: %w", err)
	}
	return &out, nil
}
