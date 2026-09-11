import { AlertTriangle, CreditCard, RefreshCw } from "lucide-react";
import type { ReactNode } from "react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { apiPost } from "@/lib/api";
import type { CodexSubscriptionView } from "@/lib/types";
import { cn, errMsg } from "@/lib/utils";

/* CodexBillingPanel — what plan a ChatGPT OAuth credential is on, when the
 * current term was paid for, whether it renews, and whether it is about to stop
 * working for a payment reason.
 *
 * Deliberately separate from the wham/usage panel next to it. Usage answers
 * "how much quota is left in this window" and moves minute to minute; this
 * answers "what was bought and until when" and moves about monthly. The one
 * thing usage can never tell you is delinquency: a delinquent account serves
 * traffic normally right up until its grace period ends, so nothing in the
 * quota view moves before it dies.
 *
 * Every judgement (plan, free, at-risk, deadline) comes from the server, which
 * computes it with cc-core's helpers — see the note on CodexSubscriptionView in
 * lib/types.ts before re-deriving any of it here. */

interface SubscriptionResponse {
  subscription?: CodexSubscriptionView;
}

function fmtDay(iso?: string | null): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "—" : d.toLocaleDateString();
}

function Row({ k, v }: { k: string; v: ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3 text-xs">
      <span className="shrink-0 text-muted-foreground">{k}</span>
      <span className="mono text-right">{v}</span>
    </div>
  );
}

export function CodexBillingPanel({
  authId,
  seed,
}: {
  authId: string;
  /* The row already carries the last probe result, so a credential checked
   * earlier renders immediately and only a manual refresh costs a round trip. */
  seed?: CodexSubscriptionView;
}) {
  const { t } = useTranslation();
  const [data, setData] = useState<CodexSubscriptionView | undefined>(seed);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  useEffect(() => {
    setData(seed);
    setErr("");
  }, [seed]);

  const run = async () => {
    setBusy(true);
    setErr("");
    try {
      const d = await apiPost<SubscriptionResponse>(
        `/admin/credentials/${encodeURIComponent(authId)}/codex-subscription`,
      );
      setData(d.subscription);
    } catch (e) {
      setErr(errMsg(e, String(e)));
    } finally {
      setBusy(false);
    }
  };

  const s = data;
  const portal = s?.info?.portal;
  const ent = s?.info?.entitlement;
  const acct = s?.info?.account;
  const last = s?.info?.last_active_subscription;
  const discount = ent?.discount;
  const delinquent = s?.risk_reason === "delinquent";
  // An app-store purchase cannot be fixed from the web portal, so it changes
  // what an operator should do about a billing problem, not just what it says.
  const offPortal =
    !!last?.purchase_origin_platform && last.purchase_origin_platform !== "chatgpt_web";
  // will_renew has two reporters; /subscriptions needs an account_id some
  // credentials don't carry, and then last_active_subscription is the only one.
  const willRenew = portal ? portal.will_renew : last?.will_renew;
  const cards = s?.info?.payment_methods ?? [];
  // A card is valid through the END of its expiry month; treating the 1st as
  // the cutoff would flag every card as dead for the month it is still good in.
  const cardExpired = (m: { exp_month?: number; exp_year?: number }) =>
    !!m.exp_year && !!m.exp_month && new Date() >= new Date(m.exp_year, m.exp_month, 1);

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-2">
          <CreditCard className="size-4 text-muted-foreground" />
          {s?.plan && (
            <Badge variant="slate" className="text-[10px] uppercase">
              {s.plan}
            </Badge>
          )}
          {s?.free && (
            <Badge
              variant="emerald"
              className="text-[10px]"
              title={s.free_reason ? t("admin.creds.billing.freeWhy", { why: s.free_reason }) : ""}
            >
              $0
            </Badge>
          )}
          {s?.at_risk && (
            <Badge variant={delinquent ? "rose" : "amber"} className="text-[10px]">
              {delinquent
                ? t("admin.creds.billing.badgeDelinquent")
                : t("admin.creds.billing.badgeNotRenewing")}
            </Badge>
          )}
          {s?.fetched_at && (
            <span className="text-[11px] text-muted-foreground">
              {t("admin.creds.billing.asOf", { when: new Date(s.fetched_at).toLocaleString() })}
            </span>
          )}
        </div>
        <Button size="sm" variant="ghost" disabled={busy} onClick={run}>
          <RefreshCw className={cn("size-3", busy && "animate-spin")} />
          {s ? t("admin.creds.billing.reprobe") : t("admin.creds.billing.probe")}
        </Button>
      </div>

      {err && (
        <div className="rounded border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive">
          {err}
        </div>
      )}

      {s?.at_risk && (
        <div
          className={cn(
            "flex gap-2 rounded-lg border px-3 py-2.5 text-xs leading-relaxed",
            delinquent
              ? "border-destructive/30 bg-destructive/10 text-destructive"
              : "border-warning/30 bg-warning/10 text-warning",
          )}
        >
          <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
          <div>
            {delinquent
              ? t("admin.creds.billing.delinquentBody", {
                  when: s.risk_deadline
                    ? fmtDay(s.risk_deadline)
                    : t("admin.creds.billing.graceEnd"),
                })
              : t("admin.creds.billing.notRenewingBody", {
                  when: s.risk_deadline ? fmtDay(s.risk_deadline) : "—",
                })}
            {offPortal && (
              <>
                {" "}
                {t("admin.creds.billing.offPortal", {
                  platform: last?.purchase_origin_platform,
                })}
              </>
            )}
          </div>
        </div>
      )}

      {s ? (
        <div className="space-y-2 rounded-xl border border-border/60 bg-card/60 p-3.5">
          <Row
            k={t("admin.creds.billing.term")}
            v={
              <>
                {fmtDay(s.purchased_at)}
                <span className="text-muted-foreground"> → </span>
                {fmtDay(s.expires_at)}
              </>
            }
          />
          <Row
            k={t("admin.creds.billing.renews")}
            v={
              <>
                {willRenew == null ? (
                  "—"
                ) : willRenew ? (
                  <span className="text-success">{t("common.yes")}</span>
                ) : (
                  <span className="text-warning">{t("common.no")}</span>
                )}
                {portal?.billing_period && (
                  <span className="text-muted-foreground">
                    {" "}
                    · {portal.billing_period}
                    {portal.billing_currency ? ` ${portal.billing_currency}` : ""}
                  </span>
                )}
              </>
            }
          />
          <Row
            k={t("admin.creds.billing.card")}
            v={
              cards.length === 0 ? (
                <span className="text-muted-foreground">{t("admin.creds.billing.cardNone")}</span>
              ) : (
                <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
                  {cards.map((m, i) => {
                    const dead = cardExpired(m);
                    return (
                      <span key={m.id ?? i} className="tabular-nums">
                        {m.last4 ? (
                          <>
                            <span className="uppercase text-muted-foreground">
                              {m.brand || m.type || "card"}
                            </span>{" "}
                            ····{m.last4}
                            {m.exp_month && m.exp_year && (
                              <span
                                className={cn(
                                  "ml-1",
                                  dead ? "text-destructive" : "text-muted-foreground",
                                )}
                              >
                                {String(m.exp_month).padStart(2, "0")}/
                                {String(m.exp_year).slice(-2)}
                                {dead ? ` · ${t("admin.creds.billing.cardExpired")}` : ""}
                              </span>
                            )}
                          </>
                        ) : (
                          <span className="text-muted-foreground">{m.type || "—"}</span>
                        )}
                      </span>
                    );
                  })}
                </span>
              )
            }
          />
          {s.free && (
            <Row
              k={t("admin.creds.billing.free")}
              v={
                <>
                  {s.free_reason === "gratis"
                    ? t("admin.creds.billing.comped")
                    : s.free_reason?.replace(/^promo:/, "") || t("common.yes")}
                  {discount?.discount_expires_at && (
                    <span className="text-muted-foreground">
                      {" "}
                      ·{" "}
                      {t("admin.creds.billing.until", {
                        when: fmtDay(discount.discount_expires_at),
                      })}
                    </span>
                  )}
                </>
              }
            />
          )}
          {!!portal?.seats_entitled && portal.seats_entitled > 1 && (
            <Row
              k={t("admin.creds.billing.seats")}
              v={`${portal.seats_in_use ?? "?"} / ${portal.seats_entitled}`}
            />
          )}
          {ent?.subscription_plan && (
            <Row k={t("admin.creds.billing.planId")} v={ent.subscription_plan} />
          )}
          {last?.purchase_origin_platform && (
            <Row k={t("admin.creds.billing.boughtVia")} v={last.purchase_origin_platform} />
          )}
          {acct?.structure && (
            <Row
              k={t("admin.creds.billing.account")}
              v={
                <>
                  {acct.structure}
                  {acct.created_time && (
                    <span className="text-muted-foreground">
                      {" "}
                      · {t("admin.creds.billing.since", { when: fmtDay(acct.created_time) })}
                    </span>
                  )}
                </>
              }
            />
          )}
          {/* Distinguishes a never-paid free account from one whose paid term
              lapsed — the two are identical on plan_type alone. */}
          {acct && !ent?.has_active_subscription && (
            <Row
              k={t("admin.creds.billing.history")}
              v={
                acct.has_previously_paid_subscription
                  ? t("admin.creds.billing.previouslyPaid")
                  : t("admin.creds.billing.neverPaid")
              }
            />
          )}
          {delinquent && portal?.grace_period_end_timestamp ? (
            <Row
              k={t("admin.creds.billing.graceEnds")}
              v={new Date(portal.grace_period_end_timestamp * 1000).toLocaleString()}
            />
          ) : null}
        </div>
      ) : (
        !err &&
        !busy && (
          <p className="text-xs text-muted-foreground">{t("admin.creds.billing.notProbed")}</p>
        )
      )}

      {busy && !s && (
        <div className="py-6 text-center text-sm text-muted-foreground">
          {t("admin.creds.billing.querying")}
        </div>
      )}
    </div>
  );
}
