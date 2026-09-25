import { motion } from "motion/react";
import type { ReactNode } from "react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { UpstreamUsagePanel } from "@/components/admin/upstream-usage-dialog";
import { Badge } from "@/components/ui/badge";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { apiGet } from "@/lib/api";
import type { Credential, CredentialUsage, UsageDay } from "@/lib/types";
import { cn, fmtInt, fmtUSD } from "@/lib/utils";
import { CodexBillingPanel } from "./codex-billing-panel";
import { credStatus, StatusDot } from "./credential-status";
import { DailyCostChart } from "./daily-cost-chart";

function Row({ k, v }: { k: string; v: ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3 text-xs">
      <span className="shrink-0 text-muted-foreground">{k}</span>
      <span className="mono truncate text-right tabular-nums">{v}</span>
    </div>
  );
}

function Block({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="rounded-xl border border-border/60 bg-card/60 p-3.5">
      <div className="eyebrow mb-2.5">{title}</div>
      {children}
    </div>
  );
}

/* CredentialDetailDialog — everything that doesn't fit on the card, split into
 * an overview (local ledger state) and a live upstream probe. Probing is a real
 * network call to the provider, so it stays behind its own tab. */
export function CredentialDetailDialog({
  cred,
  onClose,
}: {
  cred: Credential | null;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const [tab, setTab] = useState<"overview" | "upstream" | "billing">("overview");
  // The 14-day series is fetched per credential rather than shipped with the
  // list: it was ~70% of the list payload and only this dialog reads it.
  const [daily, setDaily] = useState<UsageDay[]>([]);

  const credID = cred?.id;
  useEffect(() => {
    if (!credID) return;
    setTab("overview");
    setDaily([]);
    let cancelled = false;
    apiGet<{ usage: CredentialUsage | null }>(
      `/admin/credentials/${encodeURIComponent(credID)}/usage`,
    )
      .then((r) => {
        if (!cancelled) setDaily(r.usage?.daily || []);
      })
      .catch(() => {
        // A missing sparkline is not worth a toast — the rest of the dialog
        // renders from data the list already provided.
      });
    return () => {
      cancelled = true;
    };
  }, [credID]);

  if (!cred) return null;
  const c = cred;
  const u = c.usage;
  const status = credStatus(c);
  // The upstream probe only exists for OAuth subscription accounts — API keys
  // have no per-account quota endpoint to ask.
  const canProbe = c.kind === "oauth" && (c.provider === "anthropic" || c.provider === "openai");
  // Billing is ChatGPT-only: there is no Anthropic counterpart to the
  // subscriptions/accounts-check portal.
  const canBill = c.kind === "oauth" && c.provider === "openai";
  const tabs = canBill
    ? (["overview", "upstream", "billing"] as const)
    : canProbe
      ? (["overview", "upstream"] as const)
      : (["overview"] as const);

  return (
    <Dialog open={!!cred} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle className="flex min-w-0 items-center gap-2.5">
            <StatusDot status={status} />
            <span className="truncate font-display">{c.label}</span>
            <Badge
              variant={c.kind === "apikey" ? "blue" : "slate"}
              className="shrink-0 text-[10px]"
            >
              {c.kind === "apikey" ? "API KEY" : "OAUTH"}
            </Badge>
            <span className={cn("eyebrow shrink-0 !text-[10px]", status.tone)}>
              {t(status.labelKey)}
            </span>
          </DialogTitle>
          <DialogDescription className="font-mono text-xs">{c.id}</DialogDescription>
        </DialogHeader>

        {tabs.length > 1 && (
          <div className="glass flex w-fit gap-1 rounded-xl p-1">
            {tabs.map((k) => (
              <button
                key={k}
                type="button"
                onClick={() => setTab(k)}
                className={cn(
                  "relative rounded-lg px-3.5 py-1.5 text-sm transition-colors",
                  tab === k
                    ? "text-primary-foreground"
                    : "text-muted-foreground hover:text-foreground",
                )}
              >
                {tab === k && (
                  <motion.span
                    layoutId="cred-detail-tab-pill"
                    className="absolute inset-0 -z-10 rounded-lg bg-primary"
                    transition={{ type: "spring", stiffness: 380, damping: 32 }}
                  />
                )}
                {k === "overview"
                  ? t("admin.creds.detail.tabOverview")
                  : k === "upstream"
                    ? t("admin.creds.detail.tabUpstream")
                    : t("admin.creds.detail.tabBilling")}
              </button>
            ))}
          </div>
        )}

        {tab === "overview" ? (
          <div className="grid grid-cols-1 gap-3 lg:grid-cols-3">
            <Block title={t("admin.creds.detail.usage")}>
              {u ? (
                <div className="space-y-2">
                  <Row
                    k={t("admin.creds.detail.io24h")}
                    v={`${fmtInt(u.sum_24h.input_tokens)} / ${fmtInt(u.sum_24h.output_tokens)}`}
                  />
                  {u.sum_24h.cache_read_tokens > 0 && (
                    <Row
                      k={t("admin.creds.detail.cache24h")}
                      v={fmtInt(u.sum_24h.cache_read_tokens)}
                    />
                  )}
                  <Row
                    k={t("admin.creds.detail.totalReq")}
                    v={`${fmtInt(u.total.requests)}${u.total.errors > 0 ? ` (${fmtInt(u.total.errors)} err)` : ""}`}
                  />
                  <Row k={t("admin.creds.detail.totalCost")} v={fmtUSD(u.total_cost_usd)} />
                  {u.last_used && (
                    <Row
                      k={t("admin.creds.usage.lastUsed")}
                      v={new Date(u.last_used).toLocaleString()}
                    />
                  )}
                  {c.kind === "oauth" && c.provider === "openai" && u.sum_5h && (
                    <Row
                      k={t("admin.creds.detail.rolling5h")}
                      v={`${fmtInt(u.sum_5h.input_tokens)} / ${fmtInt(u.sum_5h.output_tokens)}`}
                    />
                  )}
                </div>
              ) : (
                <p className="text-xs text-muted-foreground">{t("admin.creds.detail.noUsage")}</p>
              )}
            </Block>

            <Block title={t("admin.creds.detail.spark14")}>
              <DailyCostChart days={daily} />
            </Block>

            <Block title={t("admin.creds.detail.config")}>
              <div className="space-y-2">
                <Row
                  k={t("admin.creds.facts.proxy")}
                  v={
                    c.proxy_url || (
                      <span className="text-muted-foreground">{t("admin.creds.facts.direct")}</span>
                    )
                  }
                />
                {c.base_url && (
                  <Row
                    k={t("admin.creds.edit.baseUrl")}
                    v={<span className="break-all">{c.base_url}</span>}
                  />
                )}
                <Row
                  k={t("admin.creds.edit.maxConcurrent")}
                  v={c.max_concurrent > 0 ? String(c.max_concurrent) : "∞"}
                />
                <Row
                  k={t("admin.creds.cols.group")}
                  v={c.group || <span className="text-muted-foreground">—</span>}
                />
                <Row
                  k={t("admin.creds.detail.source")}
                  v={
                    c.file_backed
                      ? t("admin.creds.detail.sourceFile")
                      : t("admin.creds.detail.sourceConfig")
                  }
                />
              </div>
            </Block>

            {c.active_clients > 0 && c.client_tokens?.length > 0 && (
              <Block
                title={t("admin.creds.detail.activeTokens", { count: c.client_tokens.length })}
              >
                <ul className="space-y-0.5">
                  {c.client_tokens.map((tok) => (
                    <li key={tok} className="mono truncate text-xs">
                      {tok}
                    </li>
                  ))}
                </ul>
              </Block>
            )}

            {c.kind === "apikey" && (
              <div className="lg:col-span-2">
                <Block title={t("admin.creds.allowedModelsLabel")}>
                  {c.allowed_models?.length ? (
                    <ul className="flex flex-col gap-1">
                      {c.allowed_models.map((model) => (
                        <li key={model}>{model}</li>
                      ))}
                    </ul>
                  ) : (
                    t("admin.creds.allowedModelsAny")
                  )}
                </Block>
              </div>
            )}
            {c.model_map && Object.keys(c.model_map).length > 0 && (
              <div className="lg:col-span-2">
                <Block
                  title={t("admin.creds.detail.modelMap", {
                    count: Object.keys(c.model_map).length,
                  })}
                >
                  <div className="space-y-1">
                    {Object.keys(c.model_map)
                      .sort()
                      .map((k) => (
                        <div key={k} className="mono break-all text-xs leading-relaxed">
                          <span>{k}</span>
                          {c.model_map?.[k] ? (
                            <>
                              <span className="text-muted-foreground"> → </span>
                              <span>{c.model_map[k]}</span>
                            </>
                          ) : (
                            <span className="text-muted-foreground">
                              {" "}
                              ({t("admin.creds.detail.passthrough")})
                            </span>
                          )}
                        </div>
                      ))}
                  </div>
                </Block>
              </div>
            )}
          </div>
        ) : tab === "upstream" ? (
          <UpstreamUsagePanel
            authId={c.id}
            provider={c.provider === "openai" ? "openai" : "anthropic"}
          />
        ) : (
          <CodexBillingPanel authId={c.id} seed={c.codex_subscription} />
        )}
      </DialogContent>
    </Dialog>
  );
}
