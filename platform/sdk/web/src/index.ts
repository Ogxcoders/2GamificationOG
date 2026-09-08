/**
 * Universal Engagement Platform — Web SDK (TypeScript).
 *
 * Zero dependencies; browser + Node compatible. Design goals (§89/§90):
 * - Thin: no business logic. The server owns every decision.
 * - Batchy: events buffer and flush in batches (≤500, requeue on retryable failure).
 * - Idempotent: every event carries a stable client-generated key.
 * - Truthful: duplicate/rejected ingest results surface to the caller.
 */
export interface ClientConfig {
  baseUrl: string;
  apiKey: string;
  projectId: string;
  environmentId: string;
  userId: string;
  /** Batch flush interval (ms). 0 = manual flush only. Default 2000. */
  flushIntervalMs?: number;
  /** Max events per flush (server caps at 500). Default 100. */
  maxBatch?: number;
  fetchImpl?: typeof fetch;
}

export interface EventInput {
  event_type: string;
  actor_id?: string;
  subject_id?: string;
  occurred_at?: string;
  idempotency_key?: string;
  payload?: Record<string, unknown>;
  metadata?: Record<string, unknown>;
}

export interface IngestResult {
  event_id: string;
  status: 'inserted' | 'duplicate' | 'rejected';
  error?: string;
}

export interface PlayerState {
  user: Record<string, unknown>;
  tracks: Array<{ track: string; xp: number; level: number }>;
  wallets: Array<{ currency: string; balance: number }>;
  challenges: Array<{ challenge_id: string; progress: number; target: number; status: string }>;
  achievements: Array<{ achievement_id: string; unlocked_at: string }>;
  streaks: Array<{ streak_id: string; current: number; best: number; active: boolean }>;
  entitlements: Array<{ entitlement: string; expires_at?: string }>;
  leaderboards: Array<{ leaderboard_id: string; score: number; rank: number }>;
}

export class UEPError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
  ) {
    super(message);
  }
}

type PendingEvent = EventInput & {
  resolve: (r: IngestResult) => void;
  reject: (e: UEPError) => void;
};

export class UEPClient {
  private cfg: {
    baseUrl: string;
    apiKey: string;
    projectId: string;
    environmentId: string;
    userId: string;
    flushIntervalMs: number;
    maxBatch: number;
    fetchImpl?: typeof fetch;
  };
  private queue: PendingEvent[] = [];
  private timer: ReturnType<typeof setInterval> | null = null;
  private flushing = false;

  constructor(cfg: ClientConfig) {
    this.cfg = {
      ...cfg,
      flushIntervalMs: cfg.flushIntervalMs ?? 2000,
      maxBatch: cfg.maxBatch ?? 100,
    };
    if (this.cfg.flushIntervalMs > 0 && typeof setInterval === 'function') {
      this.timer = setInterval(() => void this.flush(), this.cfg.flushIntervalMs);
    }
  }

  private scope(): string {
    return `/v1/projects/${this.cfg.projectId}/environments/${this.cfg.environmentId}`;
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const f = this.cfg.fetchImpl ?? (globalThis as { fetch: typeof fetch }).fetch;
    const res = await f(`${this.cfg.baseUrl}${path}`, {
      method,
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${this.cfg.apiKey}`,
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await res.text();
    const json = text ? safeParse(text) : {};
    if (!res.ok) {
      const err = (json as { error?: { code?: string; message?: string } }).error;
      throw new UEPError(res.status, err?.code ?? 'http_error', err?.message ?? `HTTP ${res.status}`);
    }
    return json as T;
  }

  /** Track an event; resolves when the server has accepted it into the batch. */
  track(event: EventInput): Promise<IngestResult> {
    const full: EventInput = {
      actor_id: this.cfg.userId,
      occurred_at: new Date().toISOString(),
      ...event,
      idempotency_key: event.idempotency_key ?? randomKey(),
    };
    return new Promise<IngestResult>((resolve, reject) => {
      this.queue.push({ ...full, resolve, reject });
      if (this.queue.length >= this.cfg.maxBatch) void this.flush();
    });
  }

  /** Flush pending events now. Returns per-event results. */
  async flush(): Promise<IngestResult[]> {
    if (this.flushing || this.queue.length === 0) return [];
    this.flushing = true;
    const batch = this.queue.splice(0, 500);
    try {
      const res = await this.request<{ results: IngestResult[] }>('POST', `${this.scope()}/events`, {
        events: batch.map(({ resolve, reject, ...e }) => e),
      });
      const results = res.results ?? [];
      batch.forEach((p, i) => p.resolve(results[i] ?? { event_id: '', status: 'inserted' }));
      return results;
    } catch (e) {
      const uep = e instanceof UEPError ? e : new UEPError(0, 'network_error', String(e));
      // 4xx (except 429) = permanent: reject; 5xx/429/network: requeue for retry.
      if (uep.status >= 400 && uep.status < 500 && uep.status !== 429) {
        batch.forEach((p) => p.reject(uep));
      } else {
        this.queue.unshift(...batch);
      }
      throw e;
    } finally {
      this.flushing = false;
    }
  }

  /** Full player state (one call renders the whole profile). */
  async getState(userId?: string): Promise<PlayerState> {
    return this.request<PlayerState>('GET', `${this.scope()}/users/${userId ?? this.cfg.userId}/state`);
  }

  /** Leaderboard page; `me` defaults to the configured user. */
  async getLeaderboard(
    leaderboardId: string,
    opts: { window?: string; me?: string | false; limit?: number } = {},
  ): Promise<unknown> {
    const q = new URLSearchParams();
    if (opts.window) q.set('window', opts.window);
    if (opts.me !== false) q.set('me', opts.me ?? this.cfg.userId);
    if (opts.limit) q.set('limit', String(opts.limit));
    return this.request('GET', `${this.scope()}/leaderboards/${leaderboardId}?${q}`);
  }

  /** Fetch the decision trace for one event (the WHY). */
  async getTrace(eventId: string): Promise<unknown> {
    return this.request('GET', `${this.scope()}/events/${eventId}/trace`);
  }

  /** Adjust a wallet server-side (purchases, redemptions, support credits). */
  async adjustWallet(
    currency: string,
    amount: number,
    opts: { userId?: string; reference?: string; reason?: string } = {},
  ): Promise<{ balance: number; applied: boolean }> {
    return this.request('POST', `${this.scope()}/users/${opts.userId ?? this.cfg.userId}/wallets/adjust`, {
      currency,
      amount,
      reference: opts.reference,
      reason: opts.reason,
    });
  }

  /** Evaluate a feature flag for the configured user. */
  async evaluateFlag(key: string, userId?: string): Promise<{ key: string; value: unknown; source: string }> {
    const q = new URLSearchParams({ user_id: userId ?? this.cfg.userId });
    return this.request('GET', `${this.scope()}/flags/${key}/evaluate?${q}`);
  }

  /** Stop the flush timer. */
  close(): void {
    if (this.timer) clearInterval(this.timer);
    this.timer = null;
  }
}

function randomKey(): string {
  const c = globalThis.crypto;
  if (c?.randomUUID) return `idem_${c.randomUUID()}`;
  return `idem_${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`;
}

function safeParse(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return {};
  }
}
