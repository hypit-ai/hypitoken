import { Activity, Globe, Info, RefreshCw, User } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { Trans, useTranslation } from "react-i18next";
import { Link, Navigate } from "react-router-dom";
import {
  CacheGauge,
  DailyTable,
  type DayPoint,
  ModelBars,
  type ModelPoint,
  TokenDonut,
  UsageTrend,
} from "@/components/app/console-charts";
import { SpotlightCard } from "@/components/landing/interactions";
import { Reveal, RevealItem, RevealStagger } from "@/components/landing/reveal";
import { Button } from "@/components/ui/button";
import { useAuth } from "@/hooks/use-auth";
import { OverviewPanel } from "@/legacy/components/overview-panel";
import { ApiError, api } from "@/legacy/lib/api";
import type { Summary } from "@/legacy/lib/types";
import { cn, fmtDate } from "@/legacy/lib/utils";
import { apiGet } from "@/lib/api";
import { errMsg, fmtCompact, fmtUSD } from "@/lib/utils";

// PersonalAgg mirrors cc-core requestlog.Aggregate (snake_case JSON). All
// counters are this user's own — never fleet-wide.
interface PersonalAgg {
  count: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_create_tokens: number;
  cost_usd: number;
  errors: number;
  total_duration_ms: number;
}
// PersonalConsole is the /api/v2/me/console payload: account-level aggregates
// for the "个人" tab. `total` is all-time, `today` is the current UTC day.
interface PersonalConsole {
  total: PersonalAgg;
  today: PersonalAgg;
  today_key: string;
  by_model: Record<string, PersonalAgg>;
  by_day: Record<string, PersonalAgg>;
  balance_usd: number;
  // Actual paid amounts from the wallet ledger (official × group multiplier).
  // Distinct from total.cost_usd, which is the pre-discount official price.
  spent_total: number;
  spent_today: number;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function finiteNumber(value: unknown): number {
  const n = Number(value);
  return Number.isFinite(n) ? n : 0;
}

function normalizeAgg(value: unknown): PersonalAgg {
  const row = isRecord(value) ? value : {};
  return {
    count: finiteNumber(row.count),
    input_tokens: finiteNumber(row.input_tokens),
    output_tokens: finiteNumber(row.output_tokens),
    cache_read_tokens: finiteNumber(row.cache_read_tokens),
    cache_create_tokens: finiteNumber(row.cache_create_tokens),
    cost_usd: finiteNumber(row.cost_usd),
    errors: finiteNumber(row.errors),
    total_duration_ms: finiteNumber(row.total_duration_ms),
  };
}

function normalizeAggMap(value: unknown): Record<string, PersonalAgg> {
  if (!isRecord(value)) return {};
  return Object.fromEntries(Object.entries(value).map(([key, row]) => [key, normalizeAgg(row)]));
}

// The console is fed by asynchronously aggregated request logs. Treat that
// response as untrusted at runtime: a mixed-version rollout, proxy error, or
// old cached payload must degrade to zero values instead of crashing React.
function normalizePersonalConsole(value: unknown): PersonalConsole {
  const row = isRecord(value) ? value : {};
  return {
    total: normalizeAgg(row.total),
    today: normalizeAgg(row.today),
    today_key: typeof row.today_key === "string" ? row.today_key : "",
    by_model: normalizeAggMap(row.by_model),
    by_day: normalizeAggMap(row.by_day),
    balance_usd: finiteNumber(row.balance_usd),
    spent_total: finiteNumber(row.spent_total),
    spent_today: finiteNumber(row.spent_today),
  };
}

// TREND_DAYS — how many trailing UTC days the usage trend + daily table cover.
const TREND_DAYS = 14;

// buildDays turns the sparse by_day map into a continuous, oldest→newest
// series of TREND_DAYS points (missing days zero-filled) so the trend chart
// renders without gaps and the daily table has a stable window.
function buildDays(byDay: Record<string, PersonalAgg> | undefined): DayPoint[] {
  const out: DayPoint[] = [];
  const now = new Date();
  for (let i = TREND_DAYS - 1; i >= 0; i--) {
    const d = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() - i));
    const key = d.toISOString().slice(0, 10);
    const agg = byDay?.[key];
    out.push({
      day: key,
      requests: agg?.count ?? 0,
      tokens: agg ? aggTotalTokens(agg) : 0,
      cost: agg?.cost_usd ?? 0,
    });
  }
  return out;
}

const ZERO_AGG: PersonalAgg = {
  count: 0,
  input_tokens: 0,
  output_tokens: 0,
  cache_read_tokens: 0,
  cache_create_tokens: 0,
  cost_usd: 0,
  errors: 0,
  total_duration_ms: 0,
};

const aggTotalTokens = (a: PersonalAgg | undefined): number =>
  a ? a.input_tokens + a.output_tokens + a.cache_read_tokens + a.cache_create_tokens : 0;

// /app/console exposes only the original CPA-Claude OVERVIEW panel —
// charts and fleet KPIs — wrapped in the SaaS shell. Visible to any
// signed-in user since the underlying /admin/api/summary endpoint
// is GET-only and the SSO bridge admits any authenticated user. No
// credential CRUD, no token management, no requests explorer here —
// those tabs are intentionally dropped.

// MetricCell renders one tile in the KPI strip. Plain Tailwind — the
// legacy `hud-strip-grid` / `metric-cell` classes lived in CPA-Claude's
// globals.css and weren't ported over (they used Tailwind v3 utilities).
// This shadcn-style card matches the rest of the SaaS shell and lays out
// horizontally inside the parent grid.
function MetricCell({
  label,
  value,
  unit,
  hint,
  accent,
}: {
  label: string;
  value: string | number;
  unit?: string;
  hint?: string;
  accent?: boolean;
}) {
  return (
    <SpotlightCard
      tiltDeg={0}
      className={cn("w-full rounded-xl p-4", accent && "ring-1 ring-primary/30")}
    >
      <div className="text-[11px] uppercase tracking-wider text-muted-foreground">{label}</div>
      <div className="mt-2 flex items-baseline gap-1.5">
        <span
          className={cn(
            "font-mono text-2xl md:text-3xl leading-none font-semibold tabular-nums",
            accent ? "text-primary" : "text-foreground",
          )}
        >
          {value}
        </span>
        {unit && (
          <span className="font-mono text-xs text-muted-foreground uppercase tracking-wider">
            {unit}
          </span>
        )}
      </div>
      {hint && (
        <div className="mt-2 text-[11px] font-mono text-muted-foreground tabular-nums">{hint}</div>
      )}
    </SpotlightCard>
  );
}

// HealthCell renders the coarse service-health badge that replaces the old
// credential-count tile. It exposes only "can the service serve right now",
// never the underlying pool size or composition.
function HealthCell({ health }: { health?: "operational" | "down" }) {
  const { t } = useTranslation();
  const ok = health === "operational";
  const pending = health == null; // summary not loaded yet — stay neutral
  return (
    <SpotlightCard tiltDeg={0} className="w-full rounded-xl p-4 ring-1 ring-primary/30">
      <div className="text-[11px] uppercase tracking-wider text-muted-foreground">
        {t("console.metrics.serviceHealth")}
      </div>
      <div className="mt-2 flex items-center gap-2">
        <span className="relative inline-flex h-2.5 w-2.5">
          {ok && (
            <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-emerald-500 opacity-75" />
          )}
          <span
            className={cn(
              "relative inline-flex h-2.5 w-2.5 rounded-full",
              pending ? "bg-muted-foreground/40" : ok ? "bg-emerald-500" : "bg-destructive",
            )}
          />
        </span>
        <span
          className={cn(
            "font-mono text-xl md:text-2xl leading-none font-semibold",
            pending ? "text-muted-foreground" : ok ? "text-emerald-500" : "text-destructive",
          )}
        >
          {pending ? "···" : ok ? t("console.metrics.operational") : t("console.metrics.down")}
        </span>
      </div>
    </SpotlightCard>
  );
}

export default function ConsolePage() {
  const { t } = useTranslation();
  const { user } = useAuth();
  // view selects the statistics scope. Defaults to the personal (account-level)
  // view — most signed-in users care about their own usage first; the
  // platform-wide aggregate is one tap away.
  const [view, setView] = useState<"platform" | "personal">("personal");
  const [data, setData] = useState<Summary | null>(null);
  const [personal, setPersonal] = useState<PersonalConsole | null>(null);
  const [err, setErr] = useState("");
  const [lastTick, setLastTick] = useState(Date.now());
  const [refreshTick, setRefreshTick] = useState(0);
  const [refreshing, setRefreshing] = useState(false);

  const refresh = useCallback(async () => {
    try {
      if (view === "personal") {
        const p = await apiGet<unknown>("/me/console");
        setPersonal(normalizePersonalConsole(p));
      } else {
        const d = await api<Summary>("/admin/api/summary");
        setData(d);
      }
      setErr("");
      setLastTick(Date.now());
    } catch (x) {
      if (x instanceof ApiError && x.status === 401) {
        // SSO failed — JWT expired. Bounce to login.
        window.location.href = "/login";
        return;
      }
      setErr(errMsg(x, "fetch failed"));
    }
  }, [view]);

  const manualRefresh = useCallback(async () => {
    setRefreshing(true);
    await refresh();
    setRefreshTick((t) => t + 1);
    setTimeout(() => setRefreshing(false), 500);
  }, [refresh]);

  useEffect(() => {
    refresh();
    // 10s poll, paused while the tab is hidden so we don't burn cycles
    // refreshing dashboards no one is watching.
    const tick = () => {
      if (typeof document !== "undefined" && document.visibilityState === "hidden") return;
      refresh();
    };
    const t = setInterval(tick, 10000);
    const onVisible = () => {
      if (document.visibilityState === "visible") refresh();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      clearInterval(t);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [refresh]);

  if (!user) return <Navigate to="/login" replace />;

  // A single coarse service-health verb is the only platform-wide value this
  // page prints. The fleet's 24h request count and token totals used to sit
  // beside it; they are our call volume, so they are neither rendered here nor
  // sent to a non-operator any more (handleSummary drops usage_totals).
  const health = data?.service_health;

  return (
    <div className="space-y-8">
      {/* Masthead — slimmed: title + active rotation + manual refresh */}
      <header className="space-y-5">
        <div className="eyebrow flex items-center gap-2.5">
          <span className="relative inline-flex h-2 w-2">
            <span className="absolute inline-flex h-full w-full rounded-full bg-primary opacity-75 animate-ping" />
            <span className="relative inline-flex rounded-full h-2 w-2 bg-primary" />
          </span>
          <span>{t("console.eyebrow")}</span>
        </div>

        <div className="flex flex-col lg:flex-row lg:items-end lg:justify-between gap-5">
          <div className="space-y-2.5 max-w-3xl">
            <h1 className="font-display text-3xl sm:text-4xl lg:text-5xl leading-[0.95] tracking-tight">
              {view === "personal" ? (
                <User className="inline-block h-7 w-7 align-[-3px] text-primary" />
              ) : (
                <Activity className="inline-block h-7 w-7 align-[-3px] text-primary" />
              )}{" "}
              {view === "personal" ? t("console.titlePersonal") : t("console.titlePlatform")}
            </h1>
            <p className="text-sm lg:text-base text-muted-foreground max-w-2xl">
              {view === "personal"
                ? t("console.subPersonal")
                : t("console.activeWindow", { min: data ? data.active_window_minutes : "···" })}
            </p>
            <ConsoleTabs view={view} onChange={setView} />
            <div className="flex items-start gap-2 rounded-md border border-primary/30 bg-primary/[0.05] px-3 py-2 max-w-2xl">
              <Info className="h-4 w-4 shrink-0 text-primary mt-0.5" />
              <p className="text-xs text-muted-foreground leading-relaxed">
                <span className="text-foreground font-medium">
                  {view === "personal"
                    ? t("console.bannerStrongPersonal")
                    : t("console.bannerStrong")}
                </span>{" "}
                {view === "personal" ? (
                  t("console.bannerPersonal")
                ) : (
                  <Trans
                    i18nKey="console.banner"
                    components={{
                      billing: (
                        <Link
                          to="/app/billing"
                          className="underline underline-offset-2 text-foreground hover:text-primary"
                        />
                      ),
                      logs: (
                        <Link
                          to="/app/logs"
                          className="underline underline-offset-2 text-foreground hover:text-primary"
                        />
                      ),
                    }}
                  />
                )}
              </p>
            </div>
          </div>

          <div className="flex items-center gap-2 lg:justify-end">
            <Button
              variant="outline"
              onClick={manualRefresh}
              className="gap-2"
              aria-label={t("common.refresh")}
            >
              <RefreshCw
                className={cn("h-4 w-4 transition-transform", refreshing && "animate-spin")}
              />
              <span className="hidden sm:inline">{t("common.refresh")}</span>
            </Button>
            <span className="eyebrow tabular opacity-60 hidden md:inline">
              {t("console.last", { when: fmtDate(new Date(lastTick).toISOString()) })}
            </span>
          </div>
        </div>

        {err && (
          <div className="rounded-md border border-destructive/40 bg-destructive/10 px-4 py-2.5 text-sm text-destructive font-mono">
            {err}
          </div>
        )}
      </header>

      {view === "platform" ? (
        <>
          {/* Platform KPI strip — only the coarse service-health badge. The
              request / token tiles that used to sit here published our daily
              call volume to every signed-in customer; magnitudes are gone from
              this tab entirely, shapes only (see dashboard-board.tsx). */}
          <RevealStagger className="grid gap-3 grid-cols-1 sm:max-w-xs">
            <RevealItem className="flex">
              <HealthCell health={health} />
            </RevealItem>
          </RevealStagger>

          {/* The overview panel — relative-scale charts, no axes, no totals. */}
          <Reveal>
            <OverviewPanel summary={data} refreshTick={refreshTick} />
          </Reveal>
        </>
      ) : (
        <PersonalView personal={personal} />
      )}
    </div>
  );
}

// ConsoleTabs — pill switch between the platform-wide and personal scopes.
function ConsoleTabs({
  view,
  onChange,
}: {
  view: "platform" | "personal";
  onChange: (v: "platform" | "personal") => void;
}) {
  const { t } = useTranslation();
  const tabs = [
    { id: "platform" as const, label: t("console.tabs.platform"), icon: Globe },
    { id: "personal" as const, label: t("console.tabs.personal"), icon: User },
  ];
  return (
    <div className="inline-flex items-center gap-1 rounded-full border border-border bg-card/50 p-1">
      {tabs.map((tab) => {
        const Icon = tab.icon;
        const active = view === tab.id;
        return (
          <button
            type="button"
            key={tab.id}
            onClick={() => onChange(tab.id)}
            className={cn(
              "inline-flex items-center gap-1.5 rounded-full px-4 py-1.5 text-sm font-medium transition-colors",
              active
                ? "bg-primary text-primary-foreground shadow-sm"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            <Icon className="h-3.5 w-3.5" />
            {tab.label}
          </button>
        );
      })}
    </div>
  );
}

// PersonalView renders account-scoped statistics: the same KPI-tile layout as
// the platform view but driven by this user's own request log, plus a
// cumulative summary row and a per-model usage breakdown. All numbers are this
// account's — never fleet-wide.
function PersonalView({ personal }: { personal: PersonalConsole | null }) {
  const { t } = useTranslation();
  const today = personal?.today ?? ZERO_AGG;
  const total = personal?.total ?? ZERO_AGG;
  const cacheDenom = total.input_tokens + total.cache_read_tokens + total.cache_create_tokens;
  const cacheHit = cacheDenom > 0 ? (total.cache_read_tokens / cacheDenom) * 100 : 0;
  const days = buildDays(personal?.by_day);
  const modelPoints: ModelPoint[] = personal
    ? Object.entries(personal.by_model ?? {})
        .map(([model, a]) => ({
          model,
          requests: a.count,
          tokens: aggTotalTokens(a),
          cost: a.cost_usd,
        }))
        .sort((x, y) => y.tokens - x.tokens)
    : [];
  return (
    <>
      {/* KPI strip — wallet balance + today's traffic + all-time tokens. */}
      <RevealStagger className="grid gap-3 grid-cols-2 sm:grid-cols-3 lg:grid-cols-5">
        <RevealItem className="flex">
          <MetricCell
            label={t("console.metrics.balance")}
            value={fmtUSD(personal?.balance_usd)}
            accent
          />
        </RevealItem>
        <RevealItem className="flex">
          <MetricCell label={t("console.metrics.requestsToday")} value={fmtCompact(today.count)} />
        </RevealItem>
        <RevealItem className="flex">
          <MetricCell
            label={t("console.metrics.tokensInToday")}
            value={fmtCompact(today.input_tokens)}
            unit={t("console.metrics.tok")}
          />
        </RevealItem>
        <RevealItem className="flex">
          <MetricCell
            label={t("console.metrics.tokensOutToday")}
            value={fmtCompact(today.output_tokens)}
            unit={t("console.metrics.tok")}
          />
        </RevealItem>
        <RevealItem className="flex">
          <MetricCell
            label={t("console.metrics.tokensTotal")}
            value={fmtCompact(aggTotalTokens(total))}
            unit={t("console.metrics.tok")}
          />
        </RevealItem>
      </RevealStagger>

      {/* Cumulative all-time summary. */}
      <Reveal>
        <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
          <MetricCell label={t("console.personal.totalRequests")} value={fmtCompact(total.count)} />
          <MetricCell
            label={t("console.personal.totalSpent")}
            value={fmtUSD(personal?.spent_total)}
            accent
          />
          <MetricCell label={t("console.personal.cacheHit")} value={cacheHit.toFixed(1)} unit="%" />
          <MetricCell label={t("console.personal.errors")} value={fmtCompact(total.errors)} />
        </div>
      </Reveal>

      {/* Charts row — usage trend (wide) + token composition + cache gauge. */}
      <Reveal>
        <div className="grid gap-3 lg:grid-cols-4">
          <SpotlightCard tiltDeg={0} className="rounded-xl p-5 lg:col-span-2">
            <h3 className="text-sm font-semibold tracking-tight">{t("console.personal.trend")}</h3>
            <p className="mt-0.5 text-xs text-muted-foreground">
              {t("console.personal.trendSub", { n: TREND_DAYS })}
            </p>
            <div className="mt-4">
              <UsageTrend days={days} />
            </div>
          </SpotlightCard>
          <SpotlightCard tiltDeg={0} className="rounded-xl p-5">
            <h3 className="text-sm font-semibold tracking-tight">
              {t("console.personal.tokenComposition")}
            </h3>
            <div className="mt-5">
              <TokenDonut
                input={total.input_tokens}
                output={total.output_tokens}
                cacheRead={total.cache_read_tokens}
                cacheWrite={total.cache_create_tokens}
              />
            </div>
          </SpotlightCard>
          <SpotlightCard tiltDeg={0} className="rounded-xl p-5">
            <h3 className="text-sm font-semibold tracking-tight">
              {t("console.personal.cacheHit")}
            </h3>
            <CacheGauge pct={cacheHit} />
          </SpotlightCard>
        </div>
      </Reveal>

      {/* Per-model usage — horizontal bars scaled by token total. */}
      <Reveal>
        <SpotlightCard tiltDeg={0} className="rounded-xl p-5">
          <h3 className="text-sm font-semibold tracking-tight">{t("console.personal.byModel")}</h3>
          <ModelBars items={modelPoints} />
        </SpotlightCard>
      </Reveal>

      {/* Daily breakdown table. */}
      <Reveal>
        <SpotlightCard tiltDeg={0} className="rounded-xl p-5">
          <h3 className="text-sm font-semibold tracking-tight">{t("console.personal.daily")}</h3>
          <DailyTable rows={days} />
        </SpotlightCard>
      </Reveal>
    </>
  );
}
