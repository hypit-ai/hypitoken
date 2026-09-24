import {
  ArrowRight,
  ChevronDown,
  Gauge,
  Infinity as InfinityIcon,
  Minus,
  Plus,
  Receipt,
  Sparkles,
  Wallet,
} from "lucide-react";
import { AnimatePresence, motion, useReducedMotion } from "motion/react";
import { lazy, Suspense, useEffect, useMemo, useState } from "react";
import { Trans, useTranslation } from "react-i18next";
import { SpotlightCard } from "@/components/landing/interactions";
import { Reveal, RevealItem, RevealStagger } from "@/components/landing/reveal";
import { useIsMobile, usePrefersReducedMotion } from "@/components/landing/use-media";
import { api } from "@/lib/api";
import type { PricingGroup } from "@/lib/types";
import { cn } from "@/lib/utils";

const FloatingGeometry = lazy(() => import("@/components/landing/floating-geometry"));

const EASE = [0.22, 1, 0.36, 1] as const;

// ─── Catalogue-driven model rows ─────────────────────────────────────────────
//
// Rates come from GET /api/v2/pricing, which serves the same pricing.Catalog
// the billing path charges against. This file used to carry its own copy of the
// table; that second source of truth drifted, and the drift was invisible
// because nothing compared the two. claude-opus-5 and claude-fable-5 were being
// billed at $5/$25 and $10/$50 while appearing on no published price list, and
// the gpt-5.6 tiers advertised no cache-write rate although the catalogue
// charges 1.25x input for one.
//
// Nothing below may reintroduce a hardcoded RATE. That rule is absolute and is
// what this comment originally protected.
//
// What DID change: this file used to render every model the catalogue priced,
// on the reasoning that a model we bill for must appear on the price list. That
// held only because the catalogue happened to contain exactly the models on
// sale. It stopped holding on 2026-08-25, when cc-core added a price card for
// every published OpenAI SKU — not to sell them, but because pricing.Lookup
// falls back by trimming "-" segments, so a MISSING card silently bills at the
// nearest shorter name (gpt-5.4-nano was billing at the gpt-5.4 card, 12.5x
// over). Those defensive cards then surfaced here as a price list advertising
// two dozen models nobody can buy.
//
// So the catalogue and the price list answer two different questions —
// "what must never be mispriced" vs "what is on sale" — and SOLD below is the
// second one. The original protection is kept as an assertion instead of an
// emergent property: every id in SOLD must resolve to a real card
// (assertSoldModelsArePriced), so the list can never advertise a price that is
// actually a prefix-fallback guess. Adding a model to the shop means adding it
// here; that is deliberate, and it is the only honest way to keep the two in
// step now that they are no longer the same set.

/** One `<provider>/<model>` entry as served by /api/v2/pricing. */
interface PriceCard {
  input_per_1m: number;
  output_per_1m: number;
  cache_read_per_1m: number;
  cache_create_per_1m: number;
  /** Distinct 1h-TTL cache-write rate. Absent/0 means 1h bills at the 5m rate. */
  cache_create_1h_per_1m?: number;
}

interface Catalogue {
  default: PriceCard;
  provider_defaults: Record<string, PriceCard>;
  /** Keyed "<provider>/<model>", lowercase — matches pricing.Catalog.Models(). */
  models: Record<string, PriceCard>;
}

type ModelRow = {
  name: string;
  display: string;
  tier: string;
  input: number;
  output: number;
  cacheWrite: number | null;
  cacheWrite1h: number | null;
  cacheRead: number | null;
};

/** Tier label + sort weight. Keys are catalogue model ids. */
const PRESENTATION: Record<string, { display: string; tier: string }> = {
  "claude-fable-5": { display: "Claude Fable 5", tier: "flagship" },
  "claude-opus-5-5": { display: "Claude Opus 5.5", tier: "flagship" },
  "claude-opus-5": { display: "Claude Opus 5", tier: "flagship" },
  "claude-opus-4-8": { display: "Claude Opus 4.8", tier: "advanced" },
  "claude-opus-4-7": { display: "Claude Opus 4.7", tier: "advanced" },
  "claude-opus-4-6": { display: "Claude Opus 4.6", tier: "advanced" },
  "claude-sonnet-5": { display: "Claude Sonnet 5", tier: "balanced" },
  "claude-sonnet-4-6": { display: "Claude Sonnet 4.6", tier: "standard" },
  "claude-haiku-4-5": { display: "Claude Haiku 4.5", tier: "fast" },
  "gpt-6-astra": { display: "GPT-6 Astra", tier: "flagship" },
  "gpt-6-sol": { display: "GPT-6 Sol", tier: "advanced" },
  "gpt-6-luna": { display: "GPT-6 Luna", tier: "fast" },
  "gpt-5.6-sol": { display: "GPT-5.6 Sol", tier: "advanced" },
  "gpt-5.6-terra": { display: "GPT-5.6 Terra", tier: "balanced" },
  "gpt-5.6-luna": { display: "GPT-5.6 Luna", tier: "fast" },
  "gpt-5.5": { display: "GPT-5.5", tier: "standard" },
  "gpt-5.4": { display: "GPT-5.4", tier: "advanced" },
  "gpt-5.4-mini": { display: "GPT-5.4 mini", tier: "fast" },
};

// Dated snapshots (claude-sonnet-5-20260901) resolve to the same card as their
// undated base via the catalogue's prefix fallback, so listing both would show
// one model twice at one price. Keep the base, drop the snapshot.
const DATED_SUFFIX = /-\d{8}$/;

// The OpenAI models actually on sale, in display order — index 0 is the
// flagship and gets the featured card.
//
// This replaced a /^gpt-5\.\d/ pattern that tried to INFER the sold set from
// the id ("every tier shipped so far is gpt-5.<minor>"). The inference was
// wrong the moment the catalogue gained defensive cards: gpt-5.5-pro,
// gpt-5.4-nano, gpt-5.4-pro, gpt-5.2-pro, gpt-5.1 and gpt-5.6-cyber all match
// that pattern and none of them are sold. A shop's inventory is not derivable
// from a naming convention.
//
// Order is explicit rather than sorted by price because sol and gpt-5.5 carry
// the same $5.00 input rate, so a price sort put gpt-5.5 in the flagship slot
// on an alphabetical tiebreak. Say which model is the flagship rather than
// hoping the sort agrees.
//
// gpt-6-astra took the flagship slot on 2026-09-05. It is priority 1 in the
// upstream Codex catalogue and the model the CLI now defaults to, and sol moved
// down a tier rather than out — it is still sold and still the value pick at
// $5/$30 against astra's $10/$50.
//
// Deliberately NOT here: gpt-reserve and codex-auto-review. Both are
// visibility "hide" upstream, i.e. the vendor CLI never lists them, and neither
// has a published rate. A price page is an offer; do not advertise a model we
// cannot quote.
const SOLD_OPENAI = [
  "gpt-6-astra",
  "gpt-6-sol",
  "gpt-6-luna",
  "gpt-5.6-sol",
  "gpt-5.6-terra",
  "gpt-5.6-luna",
  "gpt-5.5",
  "gpt-5.4",
  "gpt-5.4-mini",
] as const;

/** Title-case a catalogue id when PRESENTATION has no entry for it. */
function deriveDisplay(model: string): string {
  return model
    .split("-")
    .map((w) => (/^[a-z]/.test(w) ? w[0].toUpperCase() + w.slice(1) : w))
    .join(" ")
    .replace(/^Claude /, "Claude ")
    .replace(/^Gpt/, "GPT");
}

/** Build one row from a catalogue id + its card. */
function rowFrom(name: string, card: PriceCard): ModelRow {
  const meta = PRESENTATION[name];
  return {
    name,
    display: meta?.display ?? deriveDisplay(name),
    // "standard" is the neutral label for a model shipped after this file was
    // last touched — it still gets listed, just without a curated tier.
    tier: meta?.tier ?? "standard",
    input: card.input_per_1m,
    output: card.output_per_1m,
    // 0 means the catalogue has no rate for that axis (OpenAI cards carry no
    // cache-write until the 5.6 line); render it as "—" rather than "$0".
    cacheWrite: card.cache_create_per_1m > 0 ? card.cache_create_per_1m : null,
    cacheWrite1h:
      card.cache_create_1h_per_1m && card.cache_create_1h_per_1m > 0
        ? card.cache_create_1h_per_1m
        : null,
    cacheRead: card.cache_read_per_1m > 0 ? card.cache_read_per_1m : null,
  };
}

/**
 * Turn the catalogue into the rows for one provider tab.
 *
 * The two providers select differently on purpose. Anthropic still lists every
 * model the catalogue prices, ordered by input rate descending — there, the
 * priced set and the sold set are still the same. OpenAI lists SOLD_OPENAI in
 * its declared order, because there the catalogue also holds defensive cards
 * for SKUs that are priced-but-not-sold (see the comment above SOLD_OPENAI).
 */
function rowsFor(cat: Catalogue | null, provider: "anthropic" | "openai"): ModelRow[] {
  if (!cat?.models) return [];

  if (provider === "openai") {
    const rows: ModelRow[] = [];
    for (const name of SOLD_OPENAI) {
      const card = cat.models[`openai/${name}`];
      // A sold model with no card would render a prefix-fallback guess as if it
      // were a published rate. Drop the row instead — a missing price is
      // visible, a wrong one is not. assertSoldModelsArePriced surfaces it in
      // development so it does not merely vanish from the page.
      if (!card) continue;
      rows.push(rowFrom(name, card));
    }
    return rows;
  }

  const rows: ModelRow[] = [];
  for (const [key, card] of Object.entries(cat.models)) {
    const slash = key.indexOf("/");
    if (slash < 0 || key.slice(0, slash) !== provider) continue;
    const name = key.slice(slash + 1);
    if (DATED_SUFFIX.test(name)) continue;
    rows.push(rowFrom(name, card));
  }
  rows.sort((a, b) => b.input - a.input || a.name.localeCompare(b.name));
  return rows;
}

/**
 * Dev-only: a model in SOLD_OPENAI with no catalogue card is a shop entry the
 * billing path cannot price. rowsFor drops it so the page never shows a
 * fallback guess as a published rate; this makes that drop loud instead of
 * silent, which is the whole reason the assertion exists.
 */
function assertSoldModelsArePriced(cat: Catalogue | null): void {
  if (!import.meta.env.DEV || !cat?.models) return;
  const missing = SOLD_OPENAI.filter((m) => !cat.models[`openai/${m}`]);
  if (missing.length > 0) {
    console.error(
      `pricing: SOLD_OPENAI lists ${missing.join(", ")} but the catalogue prices ` +
        `no such model — these rows are hidden. Add the card in cc-core ` +
        `pricing/pricing.go or remove them from SOLD_OPENAI.`,
    );
  }
}

// Format a USD/M-token rate: up to 3 decimals, trailing zeros trimmed ($5, $2.4, $0.075).
function fmtPrice(n: number): string {
  return `$${n.toFixed(3).replace(/\.?0+$/, "")}`;
}

// ─── Hero ─────────────────────────────────────────────────────────────────────

function Hero() {
  const { t } = useTranslation();
  const isMobile = useIsMobile();
  const prefersReduced = usePrefersReducedMotion();
  const showAmbient = !isMobile && !prefersReduced;

  const chips = [
    { icon: InfinityIcon, label: t("pricing.chips.noSub") },
    { icon: Wallet, label: t("pricing.chips.noMin") },
    { icon: Receipt, label: t("pricing.chips.transparent") },
  ];

  return (
    <div className="relative overflow-hidden">
      {/* ambient floating geometry, far back, very subtle */}
      {showAmbient && (
        <div
          aria-hidden
          className="pointer-events-none absolute -right-24 -top-24 h-[420px] w-[420px] opacity-70 md:opacity-100"
        >
          <Suspense fallback={null}>
            <FloatingGeometry color="#34d399" />
          </Suspense>
        </div>
      )}
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 -z-10"
        style={{
          background:
            "radial-gradient(60% 50% at 15% 0%, color-mix(in oklch, var(--primary) 12%, transparent), transparent 70%)",
        }}
      />
      <Reveal>
        <span className="eyebrow text-primary">{t("nav.pricing")}</span>
        <h1 className="mt-3 max-w-3xl font-display text-4xl font-semibold leading-[1.05] tracking-tight md:text-6xl">
          {t("pricing.title")}
        </h1>
        <p className="mt-4 max-w-2xl text-base text-muted-foreground md:text-lg">
          <Trans
            i18nKey="pricing.pageSub"
            components={{
              code: <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs" />,
            }}
          />
        </p>
      </Reveal>
      <Reveal delay={0.1} className="mt-7 flex flex-wrap gap-2.5">
        {chips.map((c) => (
          <span
            key={c.label}
            className="glass inline-flex items-center gap-2 rounded-full px-4 py-2 text-sm font-medium"
          >
            <c.icon className="h-4 w-4 text-primary" />
            {c.label}
          </span>
        ))}
      </Reveal>
    </div>
  );
}

// ─── Billing formula visualization — restrained skeuomorphic tiles ────────────

function FormulaTile({
  icon: Icon,
  label,
  hint,
  value,
  accent,
}: {
  icon: typeof Gauge;
  label: string;
  hint: string;
  value: string;
  accent?: boolean;
}) {
  return (
    <div
      className={cn(
        "glass relative flex flex-1 flex-col gap-3 rounded-2xl p-5",
        accent && "ring-1 ring-primary/30",
      )}
    >
      {/* engraved top highlight for a tactile, skeuomorphic edge */}
      <div
        aria-hidden
        className="pointer-events-none absolute inset-x-0 top-0 h-px"
        style={{
          background:
            "linear-gradient(90deg, transparent, color-mix(in oklch, var(--primary) 40%, transparent), transparent)",
        }}
      />
      <div
        className={cn(
          "inline-flex h-9 w-9 items-center justify-center rounded-lg",
          accent ? "bg-primary/15 text-primary" : "bg-muted/60 text-muted-foreground",
        )}
      >
        <Icon className="h-4.5 w-4.5" />
      </div>
      <div>
        <div className="text-xs uppercase tracking-wider text-muted-foreground">{label}</div>
        <div
          className={cn(
            "mt-1 font-display text-2xl font-semibold tracking-tight",
            accent && "text-primary",
          )}
        >
          {value}
        </div>
        <div className="mt-1 text-xs text-muted-foreground">{hint}</div>
      </div>
    </div>
  );
}

function Operator({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex shrink-0 items-center justify-center self-center py-1 md:py-0">
      <div className="flex h-9 w-9 items-center justify-center rounded-full border border-border bg-card/70 text-muted-foreground shadow-sm">
        {children}
      </div>
    </div>
  );
}

function BillingFormula() {
  const { t } = useTranslation();
  return (
    <div>
      <Reveal>
        <span className="eyebrow text-primary">{t("pricing.viz.eyebrow")}</span>
        <h2 className="mt-3 font-display text-2xl font-semibold tracking-tight md:text-3xl">
          {t("pricing.viz.title")}
        </h2>
        <p className="mt-2 max-w-2xl text-sm text-muted-foreground">{t("pricing.viz.sub")}</p>
      </Reveal>

      <RevealStagger className="mt-7 flex flex-col items-stretch gap-3 md:flex-row md:items-center">
        <RevealItem className="flex flex-1">
          <FormulaTile
            icon={Gauge}
            label={t("pricing.viz.official")}
            hint={t("pricing.viz.officialHint")}
            value="$X"
          />
        </RevealItem>
        <RevealItem>
          <Operator>
            <Plus className="h-4 w-4 rotate-45" />
          </Operator>
        </RevealItem>
        <RevealItem className="flex flex-1">
          <FormulaTile
            icon={Sparkles}
            label={t("pricing.viz.multiplier")}
            hint={t("pricing.viz.multiplierHint")}
            value="×N"
          />
        </RevealItem>
        <RevealItem>
          <Operator>
            <Minus className="h-4 w-4" />
            <Minus className="-ml-2 h-4 w-4" />
          </Operator>
        </RevealItem>
        <RevealItem className="flex flex-1">
          <FormulaTile
            icon={Wallet}
            label={t("pricing.viz.charged")}
            hint={t("pricing.viz.chargedHint")}
            value="$Y"
            accent
          />
        </RevealItem>
      </RevealStagger>

      <Reveal delay={0.1}>
        <div className="glass mt-4 flex items-center gap-2 rounded-xl px-4 py-3 font-mono text-xs text-muted-foreground sm:text-sm">
          <Receipt className="h-4 w-4 shrink-0 text-primary" />
          <span className="truncate">{t("pricing.formula")}</span>
        </div>
      </Reveal>
    </div>
  );
}

// ─── Model price bento with provider segmented control ────────────────────────

// One Input | Output price column inside a card. When the default group applies
// a discount (multiplier < 1) the official rate is struck through and the
// effective "ours" rate is emphasized in the brand color — mirroring the
// reference's official-vs-ours framing, but truthfully driven by the real
// default multiplier rather than a hardcoded discount.
function PriceCol({ label, official, mult }: { label: string; official: number; mult: number }) {
  const { t } = useTranslation();
  const discounted = mult < 1;
  return (
    <div className="flex-1 px-4 first:pl-0 last:pr-0">
      <div className="text-xs text-muted-foreground">{label}</div>
      {discounted ? (
        <>
          <div className="mt-1 flex items-center gap-1.5 text-xs text-muted-foreground">
            <span className="opacity-70">{t("pricing.official")}</span>
            <span className="font-mono line-through decoration-muted-foreground/40">
              {fmtPrice(official)}
            </span>
          </div>
          <div className="font-display text-2xl font-semibold tabular-nums text-primary">
            {fmtPrice(official * mult)}
          </div>
        </>
      ) : (
        <div className="mt-1 font-display text-2xl font-semibold tabular-nums">
          {fmtPrice(official)}
        </div>
      )}
      <div className="mt-0.5 text-[10px] uppercase tracking-wider text-muted-foreground">
        {t("pricing.unit")}
      </div>
    </div>
  );
}

// Label above value rather than beside it. Side-by-side worked while only the
// featured card showed cache rates; now that every card does, the labels have
// a quarter of the width and "缓存读取 / 缓存输入" wrapped to three lines.
function CacheRow({ label, value, mult }: { label: string; value: number | null; mult: number }) {
  return (
    <div className="rounded-lg border border-border bg-background/40 px-3 py-2">
      <div className="truncate text-[11px] leading-tight text-muted-foreground" title={label}>
        {label}
      </div>
      <div className="mt-0.5 font-mono text-sm tabular-nums">
        {value == null ? <span className="text-muted-foreground">—</span> : fmtPrice(value * mult)}
      </div>
    </div>
  );
}

function ModelCard({
  m,
  mult,
  featured,
  index,
  reduce,
}: {
  m: ModelRow;
  mult: number;
  featured?: boolean;
  index: number;
  reduce: boolean;
}) {
  const { t } = useTranslation();
  return (
    <motion.div
      initial={reduce ? false : { opacity: 0, y: 12 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ duration: 0.45, delay: index * 0.05, ease: EASE }}
      className={cn(
        "glass group relative flex flex-col overflow-hidden rounded-2xl p-5 transition-transform duration-300 hover:-translate-y-0.5",
        featured && "sm:col-span-2",
      )}
    >
      {/* engraved top highlight */}
      <div
        aria-hidden
        className="pointer-events-none absolute inset-x-0 top-0 h-px"
        style={{
          background:
            "linear-gradient(90deg, transparent, color-mix(in oklch, var(--primary) 40%, transparent), transparent)",
        }}
      />
      {/* concentric-ripple badge on the featured card */}
      {featured && (
        <div aria-hidden className="pointer-events-none absolute -right-6 -top-6 opacity-60">
          <div className="relative h-28 w-28">
            <span className="absolute inset-0 rounded-full border border-primary/15" />
            <span className="absolute inset-4 rounded-full border border-primary/10" />
            <span className="absolute inset-8 rounded-full border border-primary/10" />
          </div>
        </div>
      )}

      <div className="relative flex items-start justify-between gap-3">
        <div>
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-display text-sm font-medium text-foreground">{m.display}</span>
            <span className="rounded-full border border-border px-1.5 py-0.5 text-[10px] text-muted-foreground">
              {t(`pricing.tiers.${m.tier}`)}
            </span>
            <span className="text-xs text-muted-foreground">{t("pricing.modelTag")}</span>
          </div>
          <div
            className={cn(
              "mt-1 font-mono font-semibold text-primary",
              featured ? "text-xl md:text-2xl" : "text-base md:text-lg",
            )}
          >
            {m.name}
          </div>
        </div>
        {featured && (
          <span className="inline-flex shrink-0 items-center gap-1 rounded-full border border-primary/30 bg-primary/10 px-2 py-0.5 text-[10px] font-mono uppercase tracking-wider text-primary">
            <Sparkles className="h-3 w-3" /> {t("pricing.flagship")}
          </span>
        )}
      </div>

      {/* inner pricing sub-card: Input | Output */}
      <div className="mt-4 flex divide-x divide-border rounded-xl border border-border bg-background/40 p-4">
        <PriceCol label={t("pricing.columns.input")} official={m.input} mult={mult} />
        <PriceCol label={t("pricing.columns.output")} official={m.output} mult={mult} />
      </div>

      {/* Cache rates on every card, not just the featured one. On Claude Code
          traffic cache reads and writes are the majority of a bill, so hiding
          them on six of seven models hid most of what a customer actually pays. */}
      <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2">
        <CacheRow
          label={t(
            m.cacheWrite1h !== null ? "pricing.columns.cacheWrite5m" : "pricing.columns.cacheWrite",
          )}
          value={m.cacheWrite}
          mult={mult}
        />
        {m.cacheWrite1h !== null ? (
          <CacheRow label={t("pricing.columns.cacheWrite1h")} value={m.cacheWrite1h} mult={mult} />
        ) : null}
        <CacheRow label={t("pricing.columns.cacheRead")} value={m.cacheRead} mult={mult} />
      </div>
    </motion.div>
  );
}

function ModelBento({ models, mult }: { models: ModelRow[]; mult: number }) {
  const { t } = useTranslation();
  const reduce = useReducedMotion();
  return (
    <div>
      {/* Auto-flowing rows: the card count now follows the catalogue, so a fixed
          row count would overflow the moment a model is added. */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
        {models.map((m, i) => (
          <ModelCard
            key={m.name}
            m={m}
            mult={mult}
            featured={i === 0}
            index={i}
            reduce={!!reduce}
          />
        ))}
      </div>
      <p className="mt-4 text-xs text-muted-foreground">{t("pricing.per1m")}</p>
    </div>
  );
}

function ModelTables({
  claude,
  codex,
  claudeMult,
  codexMult,
}: {
  claude: ModelRow[];
  codex: ModelRow[];
  claudeMult: number;
  codexMult: number;
}) {
  const { t } = useTranslation();
  // Codex opens the table: it is the majority of billed traffic and carries the
  // deeper discount, so it is what a visitor arriving from the hero expects.
  const [tab, setTab] = useState<"claude" | "codex">("codex");
  const tabs = [
    {
      id: "codex" as const,
      label: t("pricing.codexTitle"),
      sub: t("pricing.codexSub"),
      models: codex,
      mult: codexMult,
    },
    {
      id: "claude" as const,
      label: t("pricing.claudeTitle"),
      sub: t("pricing.claudeSub"),
      models: claude,
      mult: claudeMult,
    },
  ];
  // biome-ignore lint/style/noNonNullAssertion: tab state is always one of the two tab ids defined above
  const active = tabs.find((x) => x.id === tab)!;

  return (
    <div>
      <Reveal>
        <span className="eyebrow text-primary">{t("pricing.tablesEyebrow")}</span>
        <div className="mt-3 flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <h2 className="font-display text-2xl font-semibold tracking-tight md:text-3xl">
              {active.label}
            </h2>
            <p className="mt-1 max-w-xl text-sm text-muted-foreground">{active.sub}</p>
          </div>
          {/* segmented control */}
          <div className="glass inline-flex rounded-full p-1 text-sm">
            {tabs.map((x) => (
              <button
                type="button"
                key={x.id}
                onClick={() => setTab(x.id)}
                className={cn(
                  "relative rounded-full px-4 py-1.5 font-medium transition-colors",
                  tab === x.id
                    ? "text-primary-foreground"
                    : "text-muted-foreground hover:text-foreground",
                )}
              >
                {tab === x.id && (
                  <motion.span
                    layoutId="pricing-tab-pill"
                    className="absolute inset-0 -z-10 rounded-full bg-primary"
                    transition={{ type: "spring", stiffness: 360, damping: 30 }}
                  />
                )}
                {x.id === "claude" ? "Claude" : "Codex"}
              </button>
            ))}
          </div>
        </div>
      </Reveal>

      <Reveal delay={0.05} className="mt-6">
        <AnimatePresence mode="wait">
          <motion.div
            key={tab}
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            transition={{ duration: 0.2 }}
          >
            <ModelBento models={active.models} mult={active.mult} />
          </motion.div>
        </AnimatePresence>
      </Reveal>
    </div>
  );
}

// ─── Access groups ────────────────────────────────────────────────────────────

function AccessGroups({ groups }: { groups: PricingGroup[] }) {
  const { t } = useTranslation();
  if (groups.length === 0) return null;
  return (
    <div>
      <Reveal>
        <span className="eyebrow text-primary">{t("pricing.accessGroupsTitle")}</span>
        <h2 className="mt-3 font-display text-2xl font-semibold tracking-tight md:text-3xl">
          {t("pricing.accessGroupsTitle")}
        </h2>
        <p className="mt-2 max-w-2xl text-sm text-muted-foreground">
          {t("pricing.accessGroupsSub")}
        </p>
      </Reveal>
      <RevealStagger className="mt-6 grid gap-4 md:grid-cols-2 lg:grid-cols-3">
        {groups.map((g) => (
          <RevealItem key={g.ID}>
            <SpotlightCard className={cn("h-full", g.IsDefault && "ring-1 ring-primary/40")}>
              <div className="flex items-center justify-between">
                <h3 className="font-display text-xl tracking-tight">{g.Name}</h3>
                {g.IsDefault && (
                  <span className="inline-flex items-center gap-1 rounded-full border border-primary/30 bg-primary/15 px-2 py-0.5 text-xs font-mono uppercase tracking-wider text-primary">
                    <Sparkles className="h-3 w-3" /> {t("pricing.defaultBadge")}
                  </span>
                )}
              </div>
              {g.Description && (
                <p className="mt-1 text-xs text-muted-foreground">{g.Description}</p>
              )}
              <div className="mt-4 space-y-2">
                <MultRow label="Codex" value={g.CodexMultiplier} />
                <MultRow label="Claude" value={g.ClaudeMultiplier} />
                <p className="pt-1 text-xs text-muted-foreground">{t("pricing.formulaCaption")}</p>
              </div>
            </SpotlightCard>
          </RevealItem>
        ))}
      </RevealStagger>
    </div>
  );
}

function MultRow({ label, value }: { label: string; value: number }) {
  const discount = value < 1;
  return (
    <div className="flex items-center justify-between rounded-lg border border-border bg-muted/30 p-3">
      <span className="text-sm text-muted-foreground">{label}</span>
      <span className="flex items-center gap-2">
        {discount && (
          <span className="rounded-full bg-primary/10 px-2 py-0.5 text-[10px] font-mono uppercase tracking-wider text-primary">
            −{Math.round((1 - value) * 100)}%
          </span>
        )}
        <span className="font-mono text-sm font-semibold tabular-nums">{value.toFixed(2)}×</span>
      </span>
    </div>
  );
}

// ─── FAQ accordion ────────────────────────────────────────────────────────────

function FaqItem({ q, a, defaultOpen }: { q: string; a: React.ReactNode; defaultOpen?: boolean }) {
  const [open, setOpen] = useState(!!defaultOpen);
  return (
    <div className="glass overflow-hidden rounded-2xl">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center justify-between gap-4 px-5 py-4 text-left"
      >
        <span className="font-medium">{q}</span>
        <ChevronDown
          className={cn(
            "h-4 w-4 shrink-0 text-muted-foreground transition-transform duration-300",
            open && "rotate-180 text-primary",
          )}
        />
      </button>
      <AnimatePresence initial={false}>
        {open && (
          <motion.div
            initial={{ height: 0, opacity: 0 }}
            animate={{ height: "auto", opacity: 1 }}
            exit={{ height: 0, opacity: 0 }}
            transition={{ duration: 0.3, ease: EASE }}
            className="overflow-hidden"
          >
            <div className="px-5 pb-5 text-sm leading-relaxed text-muted-foreground">{a}</div>
          </motion.div>
        )}
      </AnimatePresence>
    </div>
  );
}

function Faq() {
  const { t } = useTranslation();
  const logsLink = (
    // biome-ignore lint/a11y/useAnchorContent: Trans component fills in the link text from the translation string
    <a className="text-primary underline-offset-2 hover:underline" href="/app/logs" />
  );
  return (
    <div>
      <Reveal>
        <span className="eyebrow text-primary">{t("pricing.faqEyebrow")}</span>
        <h2 className="mt-3 font-display text-2xl font-semibold tracking-tight md:text-3xl">
          {t("pricing.faqTitle")}
        </h2>
      </Reveal>
      <RevealStagger className="mt-6 space-y-3">
        <RevealItem>
          <FaqItem
            defaultOpen
            q={t("pricing.faq.q1")}
            a={<Trans i18nKey="pricing.faq.a1" components={{ logs: logsLink }} />}
          />
        </RevealItem>
        <RevealItem>
          <FaqItem q={t("pricing.faq.q2")} a={t("pricing.faq.a2")} />
        </RevealItem>
        <RevealItem>
          <FaqItem q={t("pricing.faq.q3")} a={t("pricing.faq.a3")} />
        </RevealItem>
      </RevealStagger>
    </div>
  );
}

// ─── CTA ──────────────────────────────────────────────────────────────────────

function PricingCta() {
  const { t } = useTranslation();
  return (
    <Reveal>
      <div className="glass relative overflow-hidden rounded-3xl px-6 py-10 text-center md:px-12 md:py-14">
        <div
          aria-hidden
          className="pointer-events-none absolute inset-0 -z-10"
          style={{
            background:
              "radial-gradient(70% 120% at 50% 0%, color-mix(in oklch, var(--primary) 14%, transparent), transparent 70%)",
          }}
        />
        <h2 className="mx-auto max-w-2xl font-display text-2xl font-semibold tracking-tight md:text-3xl">
          {t("pricing.viz.title")}
        </h2>
        <p className="mx-auto mt-3 max-w-xl text-sm text-muted-foreground">{t("pricing.sub")}</p>
        <a
          href="/app/register"
          className="mt-6 inline-flex items-center gap-2 rounded-full bg-primary px-6 py-3 font-medium text-primary-foreground shadow-lg shadow-primary/20 transition-transform hover:scale-[1.03]"
        >
          {t("nav.signUp")}
          <ArrowRight className="h-4 w-4" />
        </a>
      </div>
    </Reveal>
  );
}

// ─── Page ─────────────────────────────────────────────────────────────────────

export default function PricingPage({ embedded }: { embedded?: boolean }) {
  const [groups, setGroups] = useState<PricingGroup[]>([]);
  const [catalogue, setCatalogue] = useState<Catalogue | null>(null);

  useEffect(() => {
    api<{ groups: PricingGroup[] }>("/groups").then((r) =>
      // 企业 VIP 分组不对外展示在价格页。
      setGroups((r.groups || []).filter((g) => g.Name !== "企业VIP")),
    );
    // Rates come from the live billing catalogue. A failure leaves the tables
    // empty rather than falling back to a stale local copy — showing no price
    // is recoverable, showing a wrong one is what this fetch exists to prevent.
    api<Catalogue>("/pricing")
      .then(setCatalogue)
      .catch(() => setCatalogue(null));
  }, []);

  const claudeRows = useMemo(() => rowsFor(catalogue, "anthropic"), [catalogue]);
  const codexRows = useMemo(() => {
    assertSoldModelsArePriced(catalogue);
    return rowsFor(catalogue, "openai");
  }, [catalogue]);

  // The default group's multipliers drive the "official vs ours" framing on the
  // model bento. Fall back to 1× (no discount) until groups load.
  const defaultGroup = groups.find((g) => g.IsDefault) ?? groups[0];
  const claudeMult = defaultGroup?.ClaudeMultiplier ?? 1;
  const codexMult = defaultGroup?.CodexMultiplier ?? 1;

  const content = (
    <div className="space-y-20 md:space-y-28">
      <Hero />
      <BillingFormula />
      <ModelTables
        claude={claudeRows}
        codex={codexRows}
        claudeMult={claudeMult}
        codexMult={codexMult}
      />
      <AccessGroups groups={groups} />
      <Faq />
      {!embedded && <PricingCta />}
    </div>
  );

  if (embedded) return content;
  return <div className="mx-auto max-w-6xl px-4 py-12 md:px-6 md:py-20">{content}</div>;
}
