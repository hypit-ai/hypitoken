// Shared TypeScript types mirroring /api/v2/* JSON shapes.

export interface User {
  id: number;
  email: string;
  role: "user" | "admin";
  balance_usd: number;
  group_id: number;
  email_verified: boolean;
  created_at: number;
  disabled?: boolean;
  // Public-arena profile (leaderboard / Agent office). Attached by /me.
  display_name?: string;
  name_is_default?: boolean;
  public_opt_in?: boolean;
  // Workspaces the user belongs to (personal first, then enterprise) with their
  // role in each. Drives the token billing-target picker + team-management nav.
  workspaces?: UserWorkspace[];
}

// A workspace membership as returned inline on /me.
export interface UserWorkspace {
  id: number;
  name: string;
  type: "personal" | "enterprise";
  role: "admin" | "member";
  // Effective billing multipliers (0-sentinel already resolved to the standard
  // default 0.3/0.05 by the server).
  claude_multiplier: number;
  codex_multiplier: number;
}

// Leaderboard / arena shapes (GET /arena/leaderboard, SSE /arena/stream).
export interface LeaderRow {
  rank: number;
  actor: string;
  name: string;
  public: boolean;
  is_you: boolean;
  tokens: number;
  requests: number;
  last_seen: number;
}

export interface LeaderboardResponse {
  metric: "tokens" | "requests";
  rows: LeaderRow[];
  you: LeaderRow | null;
}

export interface ArenaEvent {
  actor: string;
  name: string;
  public: boolean;
  provider: string;
  model: string;
  tokens: number;
  ts: number;
  is_you: boolean;
}

export interface UserProfile {
  display_name: string;
  name_is_default: boolean;
  public_opt_in: boolean;
  lifetime_tokens: number;
  lifetime_requests: number;
  last_active_at: number;
}

export interface Greeting {
  country_code: string;
  city: string;
}

export interface PricingGroup {
  ID: number;
  Name: string;
  Description: string;
  // Billing: final_charge_USD = official_USD × multiplier.
  // Defaults: claude=0.3, codex=0.05.
  CodexMultiplier: number;
  ClaudeMultiplier: number;
  CredentialGroup: string;
  IsDefault: boolean;
  CreatedAt: string;
  UpdatedAt: string;
}

export interface UserToken {
  id: number;
  token: string;
  name: string;
  daily_usd_cap: number;
  monthly_usd_cap: number;
  max_concurrent: number;
  rpm: number;
  disabled: boolean;
  last_used_at: number;
  created_at: number;
  // Priority-ordered credential-channel fallthrough list. Empty = use the
  // user's pricing group (legacy routing).
  groups?: string[];
  // Which workspace this key bills. 0 / undefined = the owner's personal space.
  workspace_id?: number;
  // Free-form labels ("研发部", "前端"). Purely for grouping spend in reports —
  // unlike `groups` they carry no routing meaning. Always an array from /tokens.
  tags?: string[];
}

/* --- Spend analytics (/me/usage/*, /workspaces/:id/usage/*) --------------- */

// One key's rollup. `email` / `user_id` are only populated in the team view.
//
// NOTE charge_events counts BILLABLE EVENTS, not requests: one call writes an
// extra row per advisor sub-model and writes none at all when the cost rounds to
// zero. Label it 计费笔数, never 请求数.
export interface TokenSpend {
  token_id: number;
  name: string;
  tags: string[];
  user_id?: number;
  email?: string;
  deleted?: boolean;
  spent_usd: number;
  charge_events: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_create_tokens: number;
  last_at?: number;
}

export interface ModelSpend {
  model: string;
  spent_usd: number;
  charge_events: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_create_tokens: number;
}

export interface TagSpend {
  tag: string;
  spent_usd: number;
  charge_events: number;
  tokens: number;
}

// One calendar day (UTC). Zero-filled server-side, so the series is dense.
export interface DaySpend {
  day: string; // YYYY-MM-DD
  spent_usd: number;
  charge_events: number;
}

export interface UsageSummary {
  range: { from: string; to: string };
  total: {
    spent_usd: number;
    charge_events: number;
    active_tokens: number;
    input_tokens: number;
    output_tokens: number;
    cache_read_tokens: number;
    cache_create_tokens: number;
  };
  by_token: TokenSpend[];
  by_model: ModelSpend[];
  by_tag: TagSpend[];
  by_day: DaySpend[];
  streak: { current_days: number; longest_days: number };
}

export interface SpendRow {
  id: number;
  created_at: number;
  token_id: number;
  token_name: string;
  token_tags: string[];
  email?: string;
  model: string;
  amount_usd: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_create_tokens: number;
  // false = a pre-v15 row whose key/model couldn't be recovered from the legacy
  // ref. Its token counts are unknown (not zero).
  attributed: boolean;
}

// Channel = a credential group with at least one usable (non-disabled,
// non-hard-failed) backing credential, as reported by /api/v2/channels.
// Powers the per-token "渠道" dropdown so users only see options that
// actually have credentials behind them.
export interface Channel {
  name: string; // group filter name ("default", "claude-official", ...)
  providers: string[]; // distinct providers backing this channel
  count: number; // usable credential count
}

export interface WalletTx {
  id: number;
  kind: "topup" | "charge" | "adjust" | "refund";
  amount_usd: number;
  ref: string;
  note: string;
  created_at: number;
}

export interface AlipayOrder {
  out_trade_no: string;
  cny_amount: number;
  usd_credit: number;
  rate: number;
  status: "pending" | "paid" | "expired" | "failed";
  trade_no: string;
  qr_code: string;
  pay_url?: string;
  img?: string;
  created_at: number;
  paid_at: number;
}

export interface ModelHealth {
  id: number;
  auth_id: string;
  provider: string;
  model: string;
  status: "ok" | "fail";
  latency_ms: number;
  error: string;
  checked_at: number;
}

export interface ExchangeRate {
  cny_per_usd: number;
  as_of: number;
}

// UsageCounts mirrors cc-core usage.Counts (token + request tallies).
export interface UsageCounts {
  requests: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  errors: number;
}

// UsageDay is one entry in the 14-day per-credential daily series (Go
// credDay). Sourced from the request log, so each day carries real USD cost.
export interface UsageDay {
  day: string; // "YYYY-MM-DD"
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_create_tokens: number;
  requests: number;
  errors: number;
  cost_usd: number;
}

// CredentialUsage mirrors the Go usageSummary struct attached to each
// credential row (total / rolling windows / daily series / lifetime cost).
export interface CredentialUsage {
  total: UsageCounts;
  sum_24h: UsageCounts;
  sum_5h: UsageCounts;
  last_used?: string;
  /** 14-day series, oldest first. Omitted by the list endpoints — only the
   * detail dialog needs it, and it was ~70% of the list payload. Fetch it via
   * GET /admin/credentials/:id/usage. */
  daily?: UsageDay[];
  total_cost_usd: number;
}

/** AllotmentSpend mirrors cc-core quotaestimate.Spend: catalogue-price USD
 * plus tokens for one span of the ledger. weighted_tokens uses the same
 * 1/1.25/0.1/5 weighting as the load balancer, in input-equivalent tokens. */
export interface AllotmentSpend {
  cost_usd: number;
  weighted_tokens: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_create_tokens: number;
  requests: number;
}

/** AllotmentMeasurement mirrors cc-core quotaestimate.Measurement: one window
 * that ran to 100%, with the spend that filled it. */
export interface AllotmentMeasurement {
  window: string;
  window_hours: number;
  window_start: string;
  reset_at: string;
  hit_at: string;
  observed_hours: number;
  spend: AllotmentSpend;
  recorded_at: string;
}

/** AllotmentEstimate mirrors cc-core quotaestimate.Estimate. The window's
 * start is resets_at − length (Anthropic's windows are fixed, not rolling);
 * observed is the ledger spend from that start to now — or to the 429 under
 * basis "quota_hit", where it is a measured 100% and full_window == observed. */
export interface AllotmentEstimate {
  window: "five_hour" | "seven_day" | string;
  window_hours: number;
  window_start: string;
  window_resets_at: string;
  /** fraction; exactly 1 under quota_hit */
  utilization: number;
  basis: "quota_hit" | "utilization" | "observed_only";
  confidence: "high" | "medium" | "low";
  observed_from: string;
  observed_to: string;
  observed_hours: number;
  observed: AllotmentSpend;
  full_window?: AllotmentSpend;
  remaining?: AllotmentSpend;
  quota_hit_at?: string;
  spend_error?: string;
}

// Credential mirrors the Go authRow struct served by
// /api/v2/admin/credentials. Time fields arrive as RFC3339 strings.
export interface Credential {
  id: string;
  kind: string; // "oauth" | "apikey"
  provider: string; // "anthropic" | "openai"
  plan_type?: string;
  label: string;
  email?: string;
  proxy_url: string;
  base_url?: string;
  group?: string;
  max_concurrent: number;
  active_clients: number;
  client_tokens: string[];
  disabled: boolean;
  quota_exceeded: boolean;
  quota_reset_at?: string;
  /**
   * API-key circuit breaker. Set while the channel is paused after repeated
   * upstream failures; the pause expires on its own and one good response
   * clears it. Absent = circuit closed.
   */
  quarantined_until?: string;
  quarantine_strikes?: number;
  expires_at?: string;
  file_backed: boolean;
  healthy: boolean;
  hard_failure: boolean;
  failure_reason?: string;
  refresh_suspended?: boolean;
  refresh_suspended_reason?: string;
  last_client_cancel?: string;
  client_cancel_reason?: string;
  model_map?: Record<string, string>;
  usage?: CredentialUsage;
  /** Rejection-anchored weekly allotment: what the 7-day window was worth
   * the last time this process saw it fill. Anthropic OAuth only; absent
   * until a weekly usage-limit 429 has been observed since start-up. */
  weekly_allotment?: AllotmentEstimate;
  /** The last few settled full-window measurements, newest first, persisted
   * across restarts. Compare consecutive entries to see an allotment shrink. */
  weekly_allotment_history?: AllotmentMeasurement[];
  /** Qualifies quota_exceeded: true when the window actually filled, false
   * when this is the pool's own throttle pause after a generic 429/401/403. */
  quota_usage_limit?: boolean;
  codex_rate_limits?: Record<string, string>;
  codex_rate_limits_at?: string;
  /** Last actively-probed chatgpt.com/backend-api/wham/usage snapshot. Shape
   * mirrors cc-core auth.CodexUsageInfo; only read by the upstream-quota panel,
   * which narrows it itself. */
  codex_usage?: unknown;
  codex_usage_at?: string;
  /** Billing view from the last codex-subscription probe. Present on the row
   * (not only in the probe response) so an account about to lapse for billing
   * reasons shows up on page load rather than only after someone clicks. */
  codex_subscription?: CodexSubscriptionView;
}

/**
 * CodexSubscriptionView mirrors the server's codexSubscriptionView. The
 * derived fields (plan/free/at_risk/…) are computed in Go by cc-core's helpers
 * and must NOT be re-derived here: "is it free" has two independent upstream
 * sources (a gratis flag and a 100%-off promo) and "is it at risk" has to pick
 * between grace-period end and term end. Recomputing either in TypeScript is
 * how the panel and the server start disagreeing about whether an account is
 * paid. Read `info` only for detail the derived fields don't cover.
 */
export interface CodexSubscriptionView {
  info?: CodexSubscriptionInfo;
  plan?: string;
  purchased_at?: string;
  expires_at?: string;
  free: boolean;
  free_reason?: string;
  at_risk: boolean;
  risk_reason?: string;
  risk_deadline?: string;
  fetched_at?: string;
}

export interface CodexDiscount {
  discount_type?: string;
  /** Percent when discount_type is "percentage" — 100 means fully free. */
  amount?: number;
  discount_expires_at?: string | null;
  promo_campaign_id?: string;
}

export interface CodexPaymentMethod {
  id?: string;
  /** "card", or another instrument type, in which case the card fields are absent. */
  type?: string;
  brand?: string;
  last4?: string;
  exp_month?: number;
  exp_year?: number;
  default?: boolean;
}

export interface CodexSubscriptionInfo {
  portal?: {
    id?: string;
    plan_type?: string;
    seats_in_use?: number;
    seats_entitled?: number;
    active_start?: string;
    active_until?: string;
    billing_period?: string;
    billing_currency?: string;
    will_renew?: boolean;
    is_delinquent?: boolean;
    grace_period_end_timestamp?: number | null;
  };
  entitlement?: {
    subscription_id?: string;
    has_active_subscription?: boolean;
    is_active_subscription_gratis?: boolean;
    subscription_plan?: string;
    expires_at?: string | null;
    renews_at?: string | null;
    cancels_at?: string | null;
    billing_period?: string;
    discount?: CodexDiscount | null;
    applied_discounts?: CodexDiscount[];
    is_delinquent?: boolean;
  };
  account?: {
    account_id?: string;
    plan_type?: string;
    structure?: string;
    created_time?: string | null;
    has_previously_paid_subscription?: boolean;
    is_deactivated?: boolean;
  };
  /** From /backend-api/payments/payment_methods. Brand + last four + expiry
   * only — Stripe never returns a full number. Worth showing because a fleet
   * usually shares a handful of cards, so one expiring card takes several
   * accounts down together and nothing else in this view groups them. */
  payment_methods?: CodexPaymentMethod[];
  last_active_subscription?: {
    subscription_id?: string;
    /** "chatgpt_web" | "ios" | "android" — an app-store purchase can't be
     * fixed from the web portal, which changes what an operator should do. */
    purchase_origin_platform?: string;
    will_renew?: boolean;
  };
  updated?: string;
}

// AdminOrder mirrors the Go db.AlipayOrder struct (no json tags → PascalCase)
// served by /admin/orders. Time fields arrive as RFC3339 strings.
export interface AdminOrder {
  OutTradeNo: string;
  UserID: number;
  CNYAmount: number;
  USDCredit: number;
  Rate: number;
  Status: string;
  TradeNo: string;
  QRCode: string;
  CreatedAt: string;
  PaidAt: string;
}

// AdminAdjustment is one fleet-wide wallet adjustment (kind='adjust') served
// by /admin/adjustments — a manual operator grant or a channel signup bonus.
// A `ref` of "signup_bonus:<slug>" marks the latter.
export interface AdminAdjustment {
  id: number;
  user_id: number;
  email: string;
  amount_usd: number;
  ref: string;
  note: string;
  created_at: number;
}

// ── Referral / gift system (internal/saas/referral) ─────────────────────────

export interface ReferralTier {
  id: number;
  campaign_id: number;
  threshold: number;
  tier_name: string;
  card_style_unlock: string;
  bonus_usd: number;
  badge: string;
}

export interface ReferralCampaign {
  id: number;
  slug: string;
  name: string;
  kind: string;
  // `status` is present on the admin campaign list, omitted from the
  // user-facing campaign payload.
  status?: string;
  invitee_bonus_usd: number;
  inviter_bonus_usd: number;
  gift_expiry_days: number;
  max_gift_usd: number;
  daily_budget_usd?: number;
  headline: string;
  subcopy: string;
  variant?: string;
  tiers: ReferralTier[];
}

export interface ReferralCard {
  id: number;
  owner_user_id: number;
  campaign_id: number;
  code: string;
  card_style: "openai" | "claude";
  card_tone: "dark" | "light";
  tagline: string;
  message: string;
  impressions: number;
  created_at: number;
}

export interface ReferralStats {
  invites: number;
  earned_usd: number;
  pending_usd: number;
  rank: number;
  current_tier: ReferralTier | null;
  next_tier: ReferralTier | null;
  next_remaining: number;
  unlocked_styles: string[];
}

export interface ReferralMe {
  stats: ReferralStats;
  card: ReferralCard;
  invite_url: string;
  invite_code: string;
  campaign: ReferralCampaign;
  /** False while `giftable_usd` is zero — bonus credit alone cannot be
   *  forwarded to another account. */
  can_send_gift: boolean;
  /** Ceiling on a single gift: the real money in the wallet (topups + gifts
   *  received − gifts sent), capped by the current balance. Bonus credit is
   *  excluded, so this is usually below the displayed balance. */
  giftable_usd: number;
}

export interface GiftCard {
  id: number;
  code: string;
  recipient_email: string;
  amount_usd: number;
  message: string;
  card_style: "openai" | "claude";
  card_tone: "dark" | "light";
  status: "pending" | "claimed" | "expired" | "refunded";
  created_at: number;
  expires_at?: number;
  claimed_at?: number;
}

// Admin referral ops dashboard.
export interface ReferralTopReferrer {
  user_id: number;
  email: string;
  invites: number;
  earned_usd: number;
}

export interface ReferralGiftTotals {
  sent_count: number;
  sent_usd: number;
  claimed_count: number;
  claimed_usd: number;
  pending_count: number;
  pending_usd: number;
  refunded_count: number;
  refunded_usd: number;
}

export interface ReferralOpsStats {
  total_users: number;
  cards_minted: number;
  impressions: number;
  conversions: number;
  fraud_blocked: number;
  inviters: number;
  platform_spend: number;
  k_factor: number;
  gift_totals: ReferralGiftTotals;
  top_referrers: ReferralTopReferrer[];
  today_bonus_usd: number;
  daily_budget_usd: number;
  budget_tripped: boolean;
}

// ---- support desk ----

export type TicketKind = "support" | "appeal" | "invoice";
export type TicketStatus = "open" | "pending" | "resolved" | "rejected";

export interface TicketMessage {
  id: number;
  author: "user" | "admin";
  body: string;
  created_at: string;
}

export interface Ticket {
  id: number;
  user_id: number;
  email: string;
  kind: TicketKind;
  subject: string;
  status: TicketStatus;
  last_actor: "user" | "admin";
  created_at: string;
  updated_at: string;
  /** Returned exactly once, when an appeal is filed without a session. */
  access_key?: string;
  /** Opaque JSON owned by whatever created the ticket; the invoice flow puts
   *  the 抬头 here so the operator panel can render copyable fields. */
  meta?: string;
  messages?: TicketMessage[];
}

export interface TicketList {
  tickets: Ticket[];
  total: number;
  limit: number;
  offset: number;
  /** Operator queue only: how many tickets are awaiting a first reply. */
  open?: number;
}

// ---- invoicing ----

export interface InvoiceTitle {
  name: string;
  tax_no: string;
  address?: string;
  phone?: string;
  bank?: string;
  bank_account?: string;
}

/** The 对公转账 destination, served from config so it can change without a rebuild. */
export interface InvoicePaymentInfo {
  account_no: string;
  account_name: string;
  bank_branch: string;
  bank_code: string;
}
