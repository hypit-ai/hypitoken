import { Activity, AlertTriangle, CheckCircle2, Clock, XCircle } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Reveal } from "@/components/landing/reveal";
import { api } from "@/lib/api";
import { cn } from "@/lib/utils";

// One aggregated availability line per provider. A slot/day is "up" if ANY
// credential of that provider was healthy in it — we only care about whether
// the service was usable, not which credential served it.
interface Slot {
  from: number; // unix seconds
  ok: number;
  total: number;
  // Real customer traffic in the slot, absent when nobody asked. The probe
  // above answers "was a synthetic request served"; these answer "did real
  // turns have to be retried", which is what a user actually felt.
  reqs?: number;
  shed?: number;
}
interface DayStat {
  date: number; // unix seconds (midnight)
  ok: number;
  total: number;
}
interface ProviderMon {
  key: string;
  name: string;
  operational: "operational" | "degraded" | "down";
  // No credential counts: the public page reports service usability only.
  checked_at: number;
  recent: Slot[];
  daily: DayStat[];
}

interface Props {
  embedded?: boolean;
}

export default function StatusPage({ embedded }: Props) {
  const { t } = useTranslation();
  const [providers, setProviders] = useState<ProviderMon[]>([]);
  const [asOf, setAsOf] = useState<number>(0);
  const [loading, setLoading] = useState(true);

  const reload = async () => {
    try {
      const r = await api<{ providers: ProviderMon[]; as_of: number }>("/health/monitor");
      setProviders(r.providers || []);
      setAsOf(r.as_of || 0);
    } catch {
      setProviders([]);
    } finally {
      setLoading(false);
    }
  };

  // biome-ignore lint/correctness/useExhaustiveDependencies: mount-only; reload is defined inline and would change every render
  useEffect(() => {
    reload();
    const id = setInterval(reload, 120_000);
    return () => clearInterval(id);
  }, []);

  const overall = computeOverall(providers);

  const content = (
    <div className="space-y-8">
      <OverallBanner overall={overall} asOf={asOf} />

      {loading && providers.length === 0 ? (
        <div className="flex items-center justify-center py-16 text-muted-foreground">
          <Activity className="mr-2 h-5 w-5 animate-pulse" />
          {t("common.loading")}
        </div>
      ) : providers.length === 0 ? (
        <EmptyState />
      ) : (
        <div className="space-y-6">
          {providers.map((p) => (
            <ProviderCard key={p.key} p={p} />
          ))}
        </div>
      )}

      <p className="text-center text-xs text-muted-foreground">
        {t("status.refreshHint", {
          n: Math.max(0, Math.round(120 - ((Date.now() / 1000 - asOf) % 120))),
        })}
      </p>
    </div>
  );

  if (embedded) {
    return (
      <div className="space-y-4">
        <h1 className="font-display text-3xl font-semibold tracking-tight">{t("nav.status")}</h1>
        {content}
      </div>
    );
  }
  return (
    <div className="mx-auto max-w-3xl px-4 py-12 md:px-6 md:py-16">
      <Reveal>
        <span className="eyebrow text-primary">{t("nav.status")}</span>
        <h1 className="mt-3 font-display text-4xl font-semibold tracking-tight md:text-5xl">
          {t("status.title")}
        </h1>
        <p className="mt-2 text-muted-foreground">{t("status.sub")}</p>
      </Reveal>
      <div className="mt-10">{content}</div>
    </div>
  );
}

// ─── Overall status banner ────────────────────────────────────────────────────

type Overall = "operational" | "degraded" | "outage" | "unknown";

function computeOverall(providers: ProviderMon[]): Overall {
  if (providers.length === 0) return "unknown";
  const op = providers.filter((p) => p.operational === "operational").length;
  if (op === providers.length) return "operational";
  if (op === 0 && providers.every((p) => p.operational === "down")) return "outage";
  return "degraded";
}

function OverallBanner({ overall, asOf }: { overall: Overall; asOf: number }) {
  const { t } = useTranslation();
  const cfg = {
    operational: { icon: CheckCircle2, key: "status.operational", bg: "bg-[#76ad2a]" },
    degraded: { icon: AlertTriangle, key: "status.degraded", bg: "bg-[#eaa82a]" },
    outage: { icon: XCircle, key: "status.outage", bg: "bg-[#e04343]" },
    unknown: { icon: Clock, key: "status.awaiting", bg: "bg-zinc-500" },
  } as const;
  const { icon: Icon, key, bg } = cfg[overall];
  const since = asOf ? new Date(asOf * 1000).toLocaleString() : "—";

  return (
    <div className={cn("rounded-md px-5 py-4 text-white shadow-sm", bg)}>
      <div className="flex items-center gap-3">
        <Icon className="h-5 w-5 shrink-0" />
        <span className="text-base font-medium md:text-lg">{t(key)}</span>
        <span className="ml-auto hidden text-xs text-white/80 sm:block">
          {t("status.updated", { when: since })}
        </span>
      </div>
    </div>
  );
}

// ─── Provider card: two aggregated uptime strips (recent 10-min + daily) ──────

const FILL_OK = "#76ad2a"; // operational green
const FILL_FAIL = "#e04343"; // outage red
const FILL_PARTIAL = "#eaa82a"; // degraded amber
const FILL_NONE = "#B0AEA5"; // no-data gray

function ProviderCard({ p }: { p: ProviderMon }) {
  const { t } = useTranslation();
  const age = p.checked_at ? Math.round(Date.now() / 1000 - p.checked_at) / 60 : null;

  return (
    <Reveal>
      <div className="glass overflow-hidden rounded-2xl p-5">
        <div className="flex items-center justify-between">
          <h2 className="font-display text-xl font-semibold">{p.name}</h2>
          <ProviderPill operational={p.operational} />
        </div>

        <div className="mt-5 space-y-5">
          <UptimeStrip
            label={t("status.recentWindow")}
            slots={p.recent.map((s) => ({
              ok: s.ok,
              total: s.total,
              ts: s.from,
              kind: "slot" as const,
              reqs: s.reqs,
              shed: s.shed,
            }))}
            slotCount={144}
          />
          <UptimeStrip
            label={t("status.dailyWindow")}
            slots={p.daily.map((d) => ({
              ok: d.ok,
              total: d.total,
              ts: d.date,
              kind: "day" as const,
            }))}
            slotCount={p.daily.length || 30}
            wide
          />
        </div>

        {age !== null && (
          <p
            className="mt-4 text-right text-[11px] text-muted-foreground"
            title={new Date(p.checked_at * 1000).toLocaleString()}
          >
            {age < 1 ? t("status.justNow") : t("status.minAgo", { n: Math.round(age) })}
          </p>
        )}
      </div>
    </Reveal>
  );
}

function ProviderPill({ operational }: { operational: ProviderMon["operational"] }) {
  const { t } = useTranslation();
  if (operational === "operational")
    return (
      <span className="rounded-full border border-emerald-300 bg-emerald-50 px-3 py-1 text-xs font-mono uppercase tracking-wider text-emerald-700 dark:border-emerald-800 dark:bg-emerald-950/40 dark:text-emerald-400">
        {t("status.pillOperational")}
      </span>
    );
  if (operational === "degraded")
    return (
      <span className="rounded-full border border-amber-300 bg-amber-50 px-3 py-1 text-xs font-mono uppercase tracking-wider text-amber-700 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-400">
        {t("status.pillDegraded")}
      </span>
    );
  return (
    <span className="rounded-full border border-red-300 bg-red-50 px-3 py-1 text-xs font-mono uppercase tracking-wider text-red-700 dark:border-red-800 dark:bg-red-950/40 dark:text-red-400">
      {t("status.pillDown")}
    </span>
  );
}

interface StripSlot {
  ok: number;
  total: number;
  ts: number;
  kind: "slot" | "day";
  reqs?: number;
  shed?: number;
}

// shedUnstable is the share of real requests upstream shed before a slot is
// drawn amber rather than green.
//
// 15% because the ordinary background is a few percent and a bad window runs
// 25-30%: low enough to catch a genuinely rough period, high enough that the
// strip is not permanently yellow. minShedSample keeps one shed out of three
// requests from painting a quiet slot.
const shedUnstable = 0.15;
const minShedSample = 8;

// slotUnstable reports whether real traffic struggled in a slot the probe
// still called healthy. A shed turn is usually retried onto another credential
// and succeeds, so it never shows up as an outage — the user just waited
// longer. Amber is the honest colour for that: not down, not fine.
function slotUnstable(s: StripSlot): boolean {
  if (s.reqs === undefined || s.shed === undefined) return false;
  if (s.reqs < minShedSample) return false;
  return s.shed / s.reqs >= shedUnstable;
}

function slotFill(s: StripSlot): string {
  if (s.total === 0) return FILL_NONE;
  if (s.kind === "day") {
    const r = s.ok / s.total;
    if (r >= 0.99) return FILL_OK;
    if (r >= 0.95) return FILL_PARTIAL;
    return FILL_FAIL;
  }
  // 10-min slot: up if any credential was healthy in the window. A slot that
  // was up but where real traffic was being shed is drawn amber — the probe
  // and the customers disagreed, and the customers are the ones to believe.
  if (s.ok === 0) return FILL_FAIL;
  return slotUnstable(s) ? FILL_PARTIAL : FILL_OK;
}

function UptimeStrip({
  label,
  slots,
  slotCount,
  wide,
}: {
  label: string;
  slots: StripSlot[];
  slotCount: number;
  wide?: boolean;
}) {
  const { t } = useTranslation();
  const BAR_W = wide ? 14 : 3;
  const BAR_GAP = wide ? 4 : 2;
  const BAR_H = 34;
  const count = slotCount;
  // Left-pad so newest is on the right.
  const padded: (StripSlot | null)[] = [
    ...Array(Math.max(0, count - slots.length)).fill(null),
    ...slots.slice(-count),
  ];
  const vbW = count * (BAR_W + BAR_GAP) - BAR_GAP;

  const withData = slots.filter((s) => s.total > 0);
  let uptimePct: number | null = null;
  if (withData.length > 0) {
    if (slots[0]?.kind === "day") {
      const ok = withData.reduce((a, s) => a + s.ok, 0);
      const tot = withData.reduce((a, s) => a + s.total, 0);
      uptimePct = tot > 0 ? (ok / tot) * 100 : null;
    } else {
      const up = withData.filter((s) => s.ok > 0).length;
      uptimePct = (up / withData.length) * 100;
    }
  }

  return (
    <div>
      <div className="mb-1.5 flex items-center justify-between text-[11px] uppercase tracking-wider text-muted-foreground">
        <span>{label}</span>
        <span className="font-mono tabular-nums normal-case">
          {uptimePct === null ? t("status.awaitingData") : `${uptimePct.toFixed(2)} %`}
        </span>
      </div>
      {/* biome-ignore lint/a11y/noSvgWithoutTitle: decorative bar chart; individual bars carry <title> elements for tooltip text */}
      <svg
        aria-hidden
        className="block w-full"
        style={{ height: BAR_H }}
        preserveAspectRatio="none"
        viewBox={`0 0 ${vbW} ${BAR_H}`}
        height={BAR_H}
      >
        {padded.map((s, i) => {
          const x = i * (BAR_W + BAR_GAP);
          const fill = s == null ? FILL_NONE : slotFill(s);
          let title = t("status.noData");
          if (s) {
            const when = new Date(s.ts * 1000).toLocaleString();
            // Show availability % only — raw ok/total would leak the
            // credential-pool size on the public page.
            const pct = s.total > 0 ? Math.round((s.ok / s.total) * 100) : null;
            title = s.total === 0 ? `${when} — ${t("status.noData")}` : `${when} — ${pct}%`;
            // An amber slot needs saying out loud: the service was up, so the
            // percentage above reads fine, and the reason it is not green is
            // that real turns were being retried.
            if (slotUnstable(s)) {
              title += ` · ${t("status.unstable", {
                pct: Math.round(((s.shed ?? 0) / (s.reqs ?? 1)) * 100),
              })}`;
            }
          }
          return (
            // biome-ignore lint/suspicious/noArrayIndexKey: positional bar chart — index IS the stable position (left=oldest, right=newest)
            <rect key={i} x={x} y={0} width={BAR_W} height={BAR_H} rx={wide ? 2 : 0} fill={fill}>
              <title>{title}</title>
            </rect>
          );
        })}
      </svg>
      <div className="mt-2 flex items-center gap-3 text-[11px] text-muted-foreground">
        <span className="shrink-0">{t("status.older")}</span>
        <span className="h-px flex-1 bg-border" />
        <span className="shrink-0">{t("status.now")}</span>
      </div>
    </div>
  );
}

function EmptyState() {
  const { t } = useTranslation();
  return (
    <div className="glass rounded-2xl p-12 text-center">
      <Clock className="mx-auto h-10 w-10 text-muted-foreground" />
      <h3 className="mt-4 font-display text-lg font-medium">{t("status.none.title")}</h3>
      <p className="mt-2 text-sm text-muted-foreground">{t("status.none.sub")}</p>
    </div>
  );
}
