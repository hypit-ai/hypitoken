import {
  AlertTriangle,
  Ban,
  CheckCircle2,
  Gauge,
  Lock,
  Pencil,
  Power,
  RefreshCw,
  ShieldOff,
  Timer,
  Trash2,
} from "lucide-react";
import { memo, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { SpotlightCard } from "@/components/landing/interactions";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import type { Credential } from "@/lib/types";
import { cn, fmtCompact, fmtHours, fmtInt, fmtUSD } from "@/lib/utils";
import { credStatus, Expiry, StatusDot } from "./credential-status";

export type CardAction = "refresh" | "clear-quota" | "clear-failure" | "toggle";

/* AlertBand — a full-bleed tinted strip between the card header and body.
 * Carries *why* a credential is degraded plus its recovery time, so an
 * operator never has to open anything to know whether to act. */
/* secondsUntil — whole seconds from now to an ISO stamp, floored at 0.
 * A throttle pause is measured in seconds, so an absolute wall-clock time is
 * the least useful way to render it; the credential list polls, so this stays
 * close enough without a per-card timer. */
function secondsUntil(iso: string): number {
  const ms = Date.parse(iso) - Date.now();
  return ms > 0 ? Math.round(ms / 1000) : 0;
}

function AlertBand({
  tone,
  icon,
  label,
  children,
}: {
  tone: "warning" | "error" | "muted";
  icon: ReactNode;
  label: string;
  children: ReactNode;
}) {
  const tones = {
    warning: "bg-warning/10 text-warning border-warning/25",
    error: "bg-destructive/10 text-destructive border-destructive/25",
    muted: "bg-muted/60 text-muted-foreground border-border",
  } as const;
  return (
    <div
      className={cn(
        "flex cursor-help items-center gap-2.5 border-b px-5 py-2 text-xs",
        tones[tone],
      )}
      title={typeof children === "string" ? children : undefined}
    >
      <span className="shrink-0">{icon}</span>
      <span className="eyebrow shrink-0 !text-[10px] !text-current">{label}</span>
      <span className="ml-auto truncate text-right font-mono opacity-90">{children}</span>
    </div>
  );
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="eyebrow mb-1">{label}</dt>
      <dd className="mono truncate text-sm">{children}</dd>
    </div>
  );
}

// memo + credential-typed callbacks. The list polls every 30s and swaps in a
// fresh array, so without this every card in the grid re-rendered on each tick.
// The callbacks take the credential rather than closing over it so the parent
// can hand every card the same stable function — per-card arrow props would
// change identity on every parent render and defeat the memo entirely.
export const CredentialCard = memo(function CredentialCard({
  cred: c,
  busy,
  onEdit,
  onDetail,
  onDelete,
  onAction,
}: {
  cred: Credential;
  busy: boolean;
  onEdit: (c: Credential) => void;
  onDetail: (c: Credential) => void;
  onDelete: (c: Credential) => void;
  onAction: (c: Credential, kind: CardAction) => void;
}) {
  const { t } = useTranslation();
  const status = credStatus(c);
  const u = c.usage;
  const slotPct =
    c.max_concurrent > 0
      ? Math.min(100, Math.round((c.active_clients / c.max_concurrent) * 100))
      : 0;
  const mapped = c.model_map ? Object.keys(c.model_map).length : 0;
  const cancelledRecently =
    !!c.last_client_cancel && Date.now() - new Date(c.last_client_cancel).getTime() < 3600 * 1000;
  // config.yaml-declared credentials are read-only: the backend rejects PATCH
  // with 400, so offering the controls would only produce a confusing error.
  const readOnly = !c.file_backed;

  return (
    <SpotlightCard
      padded={false}
      spotlight={false}
      className={cn("flex h-full flex-col", c.disabled && "opacity-70")}
    >
      {/* accent hairline — only lit while the credential can actually serve */}
      <div
        aria-hidden
        className={cn(
          "absolute inset-x-0 top-0 z-10 h-[2px] transition-opacity",
          status.live
            ? "bg-gradient-to-r from-transparent via-primary/50 to-transparent opacity-70 group-hover:opacity-100"
            : "opacity-0",
        )}
      />

      <div className="flex items-start justify-between gap-3 border-b border-border/60 px-5 py-4">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <StatusDot status={status} />
            <h3 className="truncate font-display text-lg font-semibold leading-tight tracking-tight">
              {c.label}
            </h3>
          </div>
          <p className="mono mt-1 truncate pl-4 text-[11px] text-muted-foreground">{c.id}</p>
          {c.email && (
            <p className="mt-0.5 truncate pl-4 text-[11px] text-muted-foreground">{c.email}</p>
          )}
        </div>
        <div className="flex shrink-0 flex-col items-end gap-1">
          <span className={cn("eyebrow !text-[10px]", status.tone)}>{t(status.labelKey)}</span>
          <div className="flex flex-wrap justify-end gap-1">
            <Badge
              variant={c.kind === "apikey" ? "blue" : "slate"}
              className="px-2 py-0 text-[10px]"
            >
              {c.kind === "apikey" ? "API KEY" : "OAUTH"}
            </Badge>
            {c.plan_type && (
              <Badge variant="violet" className="px-2 py-0 text-[10px] uppercase">
                {c.plan_type}
              </Badge>
            )}
            {c.group && (
              <Badge variant="amber" className="max-w-[120px] truncate px-2 py-0 text-[10px]">
                {c.group}
              </Badge>
            )}
            {readOnly && (
              <Badge
                variant="slate"
                className="px-2 py-0 text-[10px]"
                title={t("admin.creds.actions.readonlyHint")}
              >
                <Lock className="mr-1 size-2.5" />
                {t("admin.creds.actions.readonly")}
              </Badge>
            )}
          </div>
        </div>
      </div>

      {c.quota_exceeded &&
        (c.quota_usage_limit ? (
          <AlertBand
            tone="warning"
            icon={<AlertTriangle className="size-3.5" />}
            label={t("admin.creds.alerts.quotaTitle")}
          >
            {c.quota_reset_at
              ? t("admin.creds.alerts.quotaReset", {
                  at: new Date(c.quota_reset_at).toLocaleString(),
                })
              : t("admin.creds.alerts.quotaNoReset")}
          </AlertBand>
        ) : (
          /* Upstream throttling, not a filled window. Muted and worded so the
           * operator leaves it alone: it expires by itself, and the request
           * that hit it has already been retried on another credential. */
          <AlertBand
            tone="muted"
            icon={<Timer className="size-3.5" />}
            label={t("admin.creds.alerts.throttleTitle")}
          >
            {c.quota_reset_at
              ? t("admin.creds.alerts.throttleBody", { s: secondsUntil(c.quota_reset_at) })
              : t("admin.creds.alerts.throttleNoReset")}
          </AlertBand>
        ))}
      {c.quarantined_until && (
        <AlertBand
          tone="warning"
          icon={<AlertTriangle className="size-3.5" />}
          label={t("admin.creds.alerts.pausedTitle")}
        >
          {t("admin.creds.alerts.pausedBody", {
            at: new Date(c.quarantined_until).toLocaleString(),
            round: c.quarantine_strikes || 1,
          })}
        </AlertBand>
      )}
      {c.failure_reason && !c.quota_exceeded && (
        <AlertBand
          tone={c.hard_failure ? "error" : "warning"}
          icon={
            c.hard_failure ? (
              <ShieldOff className="size-3.5" />
            ) : (
              <AlertTriangle className="size-3.5" />
            )
          }
          label={
            c.hard_failure
              ? t("admin.creds.alerts.hardFailTitle")
              : t("admin.creds.alerts.degradedTitle")
          }
        >
          {c.failure_reason}
        </AlertBand>
      )}
      {/* Billing risk is the only failure mode with no other tell: a delinquent
          ChatGPT account serves traffic normally until its grace period ends,
          so nothing in health or quota moves before it dies. Surfaced on the
          card so it is visible without opening the billing tab. */}
      {c.codex_subscription?.at_risk && (
        <AlertBand
          tone={c.codex_subscription.risk_reason === "delinquent" ? "error" : "warning"}
          icon={<AlertTriangle className="size-3.5" />}
          label={
            c.codex_subscription.risk_reason === "delinquent"
              ? t("admin.creds.billing.badgeDelinquent")
              : t("admin.creds.billing.badgeNotRenewing")
          }
        >
          {c.codex_subscription.risk_deadline
            ? t("admin.creds.billing.riskUntil", {
                when: new Date(c.codex_subscription.risk_deadline).toLocaleDateString(),
              })
            : t("admin.creds.billing.riskSoon")}
        </AlertBand>
      )}
      {c.refresh_suspended && (
        <AlertBand
          tone="error"
          icon={<ShieldOff className="size-3.5" />}
          label={t("admin.creds.alerts.frozenTitle")}
        >
          {c.refresh_suspended_reason ||
            (c.disabled
              ? t("admin.creds.alerts.frozenDisabled")
              : t("admin.creds.alerts.frozenHard"))}
        </AlertBand>
      )}
      {cancelledRecently && c.last_client_cancel && (
        <AlertBand
          tone="muted"
          icon={<Ban className="size-3.5" />}
          label={t("admin.creds.alerts.cancelTitle")}
        >
          {new Date(c.last_client_cancel).toLocaleTimeString()}
          {c.client_cancel_reason ? ` · ${c.client_cancel_reason}` : ""}
        </AlertBand>
      )}

      <dl className="grid grid-cols-2 gap-x-6 gap-y-3.5 px-5 py-4">
        <div className="min-w-0">
          <dt className="eyebrow mb-1">{t("admin.creds.facts.slots")}</dt>
          {/* The utilisation bar lives inside the <dd>: a <dl> group may only
              contain dt/dd pairs, so a sibling <div> here fails a11y. */}
          <dd className="mono text-sm tabular-nums">
            {c.active_clients}/{c.max_concurrent > 0 ? c.max_concurrent : "∞"}
            {c.max_concurrent > 0 && (
              <span className="mt-1.5 block h-1 w-full max-w-[120px] overflow-hidden rounded-full bg-muted">
                <span
                  className={cn(
                    "block h-full transition-all",
                    slotPct > 80 ? "bg-warning" : "bg-success",
                  )}
                  style={{ width: `${slotPct}%` }}
                />
              </span>
            )}
          </dd>
        </div>
        <Fact label={t("admin.creds.cols.expires")}>
          <Expiry cred={c} />
        </Fact>
        <Fact label={t("admin.creds.facts.proxy")}>
          {c.proxy_url || (
            <span className="text-muted-foreground">{t("admin.creds.facts.direct")}</span>
          )}
        </Fact>
        {c.kind === "apikey" && (
          <Fact label={t("admin.creds.allowedModelsLabel")}>
            {c.allowed_models?.length
              ? t("admin.creds.allowedModelsCount", { count: c.allowed_models.length })
              : t("admin.creds.allowedModelsAny")}
          </Fact>
        )}
        <Fact label={t("admin.creds.facts.modelMap")}>
          {mapped > 0 ? (
            t("admin.creds.facts.modelMapCount", { count: mapped })
          ) : (
            <span className="text-muted-foreground">—</span>
          )}
        </Fact>
      </dl>

      <div className="mt-auto grid grid-cols-3 gap-4 border-y border-border/60 bg-muted/30 px-5 py-3.5">
        <div className="min-w-0">
          <div className="eyebrow mb-1 truncate !tracking-[0.09em]">
            {t("admin.creds.usage.h24")}
          </div>
          <div className="mono truncate text-sm tabular-nums">
            {u ? `${fmtInt(u.sum_24h.input_tokens)} / ${fmtInt(u.sum_24h.output_tokens)}` : "—"}
          </div>
        </div>
        <div className="min-w-0">
          <div className="eyebrow mb-1 truncate !tracking-[0.09em]">
            {t("admin.creds.usage.lifetime")}
          </div>
          <div className="mono truncate text-sm tabular-nums">
            {u ? fmtUSD(u.total_cost_usd) : "—"}
          </div>
          {u && (
            <div className="mono truncate text-[11px] text-muted-foreground tabular-nums">
              {fmtInt(u.total.requests)}
              {u.total.errors > 0 && (
                <span className="text-destructive"> ({fmtInt(u.total.errors)})</span>
              )}
            </div>
          )}
        </div>
        <div className="min-w-0">
          <div className="eyebrow mb-1 truncate !tracking-[0.09em]">
            {t("admin.creds.usage.lastUsed")}
          </div>
          <div className="mono truncate text-sm">
            {u?.last_used ? new Date(u.last_used).toLocaleDateString() : "—"}
          </div>
        </div>
      </div>

      {c.weekly_allotment?.full_window && (
        <div className="border-b border-border/60 bg-muted/30 px-5 py-2.5">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div className="eyebrow truncate !tracking-[0.09em]">
              {t("admin.creds.usage.weeklyAllotment")}
            </div>
            <div className="mono text-sm tabular-nums">
              <span className="font-semibold">
                ≈ {fmtUSD(c.weekly_allotment.full_window.cost_usd)}
              </span>
              <span className="text-[11px] text-muted-foreground">
                {" "}
                · {fmtCompact(c.weekly_allotment.full_window.weighted_tokens)} wtok
              </span>
            </div>
          </div>
          <div className="mt-0.5 text-[11px] text-muted-foreground">
            {t("admin.creds.usage.weeklyAllotmentHint", {
              at: c.weekly_allotment.quota_hit_at
                ? new Date(c.weekly_allotment.quota_hit_at).toLocaleString()
                : "—",
              hours: fmtHours(c.weekly_allotment.observed_hours),
            })}
          </div>
          {(() => {
            const cur = c.weekly_allotment?.quota_hit_at;
            const earlier = (c.weekly_allotment_history || [])
              .filter((m) => m.hit_at !== cur)
              .slice(0, 4);
            if (!earlier.length) return null;
            return (
              <div className="mt-1 space-y-0.5">
                {earlier.map((m) => (
                  <div
                    key={m.hit_at}
                    className="mono flex justify-between gap-2 text-[11px] text-muted-foreground tabular-nums"
                  >
                    <span>
                      {t("admin.creds.usage.weeklyHistoryRow", {
                        at: new Date(m.hit_at).toLocaleDateString(),
                      })}
                    </span>
                    <span>
                      ≈ {fmtUSD(m.spend.cost_usd)} · {fmtCompact(m.spend.weighted_tokens)} wtok ·{" "}
                      {fmtHours(m.observed_hours)}
                    </span>
                  </div>
                ))}
              </div>
            );
          })()}
        </div>
      )}
      <div className="flex flex-wrap items-center gap-1.5 px-5 py-3">
        <Button
          size="sm"
          variant="outline"
          onClick={() => onDetail(c)}
          title={t("admin.creds.actions.detail")}
        >
          <Gauge className="size-3.5" />
          {t("admin.creds.actions.detail")}
        </Button>
        {!readOnly && (
          <>
            <Button size="sm" variant="outline" onClick={() => onEdit(c)} title={t("common.edit")}>
              <Pencil className="size-3.5" />
            </Button>
            <Button
              size="sm"
              variant="outline"
              disabled={busy}
              onClick={() => onAction(c, "toggle")}
              title={
                c.disabled ? t("admin.creds.actions.enable") : t("admin.creds.actions.disable")
              }
            >
              <Power className={cn("size-3.5", c.disabled && "text-muted-foreground")} />
            </Button>
            {c.kind === "oauth" && (
              <Button
                size="sm"
                variant="outline"
                disabled={busy}
                onClick={() => onAction(c, "refresh")}
                title={t("admin.creds.actions.refresh")}
              >
                <RefreshCw className={cn("size-3.5", busy && "animate-spin")} />
              </Button>
            )}
            {c.quota_exceeded && (
              <Button
                size="sm"
                variant="warning"
                disabled={busy}
                onClick={() => onAction(c, "clear-quota")}
              >
                <CheckCircle2 className="size-3.5" />
                {t("admin.creds.actions.clearQuota")}
              </Button>
            )}
            {(c.hard_failure || (!c.healthy && !c.quota_exceeded && !c.disabled)) && (
              <Button
                size="sm"
                variant="warning"
                disabled={busy}
                onClick={() => onAction(c, "clear-failure")}
              >
                <CheckCircle2 className="size-3.5" />
                {t("admin.creds.actions.markHealthy")}
              </Button>
            )}
            <Button
              size="sm"
              variant="outline"
              className="ml-auto border-destructive/40 text-destructive hover:bg-destructive/10 hover:text-destructive"
              onClick={() => onDelete(c)}
              title={t("common.delete")}
            >
              <Trash2 className="size-3.5" />
            </Button>
          </>
        )}
      </div>
    </SpotlightCard>
  );
});
