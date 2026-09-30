import {
  Building2,
  Gift,
  KeyRound,
  LayoutDashboard,
  LifeBuoy,
  Megaphone,
  Receipt,
  ScrollText,
  Shield,
  ShoppingCart,
  Sparkles,
  Users,
} from "lucide-react";
import { motion } from "motion/react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { NavLink, Route, Routes, useLocation } from "react-router-dom";
import { toast } from "sonner";
import { AdminBackdrop } from "@/components/admin/admin-backdrop";
import { AdminDashboard } from "@/components/admin/admin-dashboard";
import { AttributionTab } from "@/components/admin/attribution-tab";
import { CredentialsTab } from "@/components/admin/credentials/credentials-tab";
import { FadeIn } from "@/components/admin/fade-in";
import { InvoicesTab } from "@/components/admin/invoices-tab";
import { OverviewPanel } from "@/components/admin/overview-panel";
import { ReferralTab } from "@/components/admin/referral-tab";
import { RequestsExplorer } from "@/components/admin/requests-explorer";
import { TicketsTab } from "@/components/admin/tickets-tab";
import { WorkspacesTab } from "@/components/admin/workspaces-tab";
import { GlassPanel, PageHeader } from "@/components/app/page-primitives";
import { Pager } from "@/components/app/pager";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { apiGet, apiPatch, apiPost } from "@/lib/api";
import type { AdminAdjustment, AdminOrder, User } from "@/lib/types";
import { cn, errMsg, fmtUSD } from "@/lib/utils";

const TABS = [
  { to: "dashboard", labelKey: "admin.tabs.dashboard", icon: Sparkles },
  { to: "fleet", labelKey: "admin.tabs.fleet", icon: LayoutDashboard },
  { to: "users", labelKey: "admin.tabs.users", icon: Users },
  { to: "workspaces", labelKey: "admin.tabs.workspaces", icon: Building2 },
  { to: "credentials", labelKey: "admin.tabs.credentials", icon: KeyRound },
  { to: "requests", labelKey: "admin.tabs.requests", icon: ScrollText },
  { to: "payments", labelKey: "admin.tabs.payments", icon: ShoppingCart },
  { to: "growth", labelKey: "admin.tabs.growth", icon: Megaphone },
  { to: "referral", labelKey: "admin.tabs.referral", icon: Gift },
  { to: "tickets", labelKey: "admin.tabs.tickets", icon: LifeBuoy },
  { to: "invoices", labelKey: "admin.tabs.invoices", icon: Receipt },
];

export default function AdminPage() {
  const { t } = useTranslation();
  // Auto-refresh tick. The fleet/overview panels load once on mount, so a
  // credential whose quota cooldown has expired (cleared server-side on the
  // next Snapshot) would keep showing a stale "配额耗尽" badge until a manual
  // reload. Bump a tick every 15s so those panels re-fetch and reflect live
  // server state. The requests log is intentionally left manual — auto-
  // refreshing it mid-investigation is disruptive.
  const [tick, setTick] = useState(0);
  useEffect(() => {
    const id = setInterval(() => setTick((n) => n + 1), 15000);
    return () => clearInterval(id);
  }, []);
  return (
    <>
      {/* Fixed ambient scenery; outside the space-y flow so it adds no margin.
          Backdrop paints at z-0 (above the shell's opaque bg-background),
          content is lifted to z-10 so it stays on top of the scenery. */}
      <AdminBackdrop />
      <div className="relative z-10 space-y-6">
        <PageHeader
          eyebrow={t("nav.operator")}
          icon={Shield}
          title={t("admin.panelTitle")}
          sub={t("admin.panelSub")}
          animate={false}
        />

        <AdminTabBar />

        <Routes>
          <Route index element={<AdminDashboard />} />
          <Route path="dashboard" element={<AdminDashboard />} />
          <Route path="fleet" element={<OverviewPanel refreshTick={tick} />} />
          {/* legacy alias — earlier deeplinks pointed at /overview */}
          <Route path="overview" element={<OverviewPanel refreshTick={tick} />} />
          <Route path="users" element={<UsersTab />} />
          <Route path="workspaces" element={<WorkspacesTab />} />
          <Route path="credentials" element={<CredentialsTab />} />
          <Route path="requests" element={<RequestsExplorer refreshTick={0} />} />
          <Route path="payments" element={<PaymentsTab />} />
          <Route path="growth" element={<AttributionTab />} />
          <Route path="referral" element={<ReferralTab />} />
          <Route path="tickets" element={<TicketsTab />} />
          <Route path="invoices" element={<InvoicesTab />} />
        </Routes>
      </div>
    </>
  );
}

// AdminTabBar — a glass tab strip whose active tab carries a shared underline
// pill that slides between tabs via a motion layoutId, the same language as the
// app sidebar. Horizontally scrollable on narrow screens; never wraps.
function AdminTabBar() {
  const { t } = useTranslation();
  const { pathname } = useLocation();
  const isActive = (to: string) =>
    to === "dashboard"
      ? pathname === "/app/admin" || pathname === "/app/admin/dashboard"
      : pathname === `/app/admin/${to}` || pathname.startsWith(`/app/admin/${to}/`);
  return (
    <div className="glass no-scrollbar flex gap-1 overflow-x-auto rounded-xl p-1">
      {TABS.map((tab) => {
        const active = isActive(tab.to);
        return (
          <NavLink
            key={tab.to}
            to={`/app/admin/${tab.to}`}
            end={tab.to === "dashboard"}
            className={cn(
              "relative inline-flex shrink-0 items-center gap-2 rounded-lg px-3.5 py-2 text-sm transition-colors",
              active ? "text-primary-foreground" : "text-muted-foreground hover:text-foreground",
            )}
          >
            {active && (
              <motion.span
                layoutId="admin-tab-pill"
                className="absolute inset-0 -z-10 rounded-lg bg-primary shadow-[0_8px_24px_-12px_color-mix(in_oklch,var(--primary)_70%,transparent)]"
                transition={{ type: "spring", stiffness: 380, damping: 32 }}
              />
            )}
            <tab.icon className="h-3.5 w-3.5" />
            {t(tab.labelKey)}
          </NavLink>
        );
      })}
    </div>
  );
}

const USERS_PAGE = 50;

function UsersTab() {
  const { t } = useTranslation();
  const [users, setUsers] = useState<User[]>([]);
  // qInput is the live textbox; q is the debounced value that actually hits
  // the API, so we don't fire one request per keystroke.
  const [qInput, setQInput] = useState("");
  const [q, setQ] = useState("");
  const [offset, setOffset] = useState(0);
  const [total, setTotal] = useState<number | undefined>(undefined);
  const [busy, setBusy] = useState(true);
  // user id with an in-flight row mutation — disables that row's controls.
  const [rowBusy, setRowBusy] = useState<number | null>(null);

  useEffect(() => {
    const id = setTimeout(() => {
      setQ(qInput);
      setOffset(0); // a new search always starts from page 1
    }, 300);
    return () => clearTimeout(id);
  }, [qInput]);

  const reload = async () => {
    setBusy(true);
    try {
      const u = await apiGet<{ users: User[]; total?: number }>(
        `/admin/users?q=${encodeURIComponent(q)}&limit=${USERS_PAGE}&offset=${offset}`,
      );
      setUsers(u.users || []);
      setTotal(u.total);
    } catch (e) {
      toast.error(errMsg(e));
    } finally {
      setBusy(false);
    }
  };
  // biome-ignore lint/correctness/useExhaustiveDependencies: reload closes over q/offset; re-run only when the search query or page changes.
  useEffect(() => {
    reload();
  }, [q, offset]);

  // Shared row mutation: optimistic disable via rowBusy, then refresh the
  // current page so the controlled select/badges reflect server truth.
  const patchUser = async (id: number, body: Record<string, unknown>, okMsg: string) => {
    setRowBusy(id);
    try {
      await apiPatch(`/admin/users/${id}`, body);
      toast.success(okMsg);
      await reload();
    } catch (e) {
      toast.error(errMsg(e));
    } finally {
      setRowBusy(null);
    }
  };

  return (
    <FadeIn>
      <GlassPanel
        title={t("admin.users.headingCount", { n: total ?? users.length })}
        action={
          <Input
            placeholder={t("admin.users.searchPlaceholder")}
            value={qInput}
            onChange={(e) => setQInput(e.target.value)}
            className="w-full sm:w-64"
          />
        }
        bodyClassName="p-0"
      >
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("admin.users.cols.email")}</TableHead>
              <TableHead>{t("admin.users.cols.role")}</TableHead>
              <TableHead className="text-right">{t("admin.users.cols.balance")}</TableHead>
              <TableHead></TableHead>
              <TableHead></TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {users.length === 0 && (
              <TableRow>
                <TableCell colSpan={5} className="py-12 text-center text-muted-foreground">
                  {busy ? t("common.loading") : t("admin.users.empty")}
                </TableCell>
              </TableRow>
            )}
            {users.map((u) => (
              <TableRow key={u.id} className={u.disabled ? "opacity-50" : ""}>
                <TableCell className="font-medium">{u.email}</TableCell>
                <TableCell>
                  <span
                    className={`rounded border px-2 py-0.5 text-xs font-mono uppercase tracking-wider ${u.role === "admin" ? "border-primary/40 bg-primary/15 text-primary" : "border-border bg-muted"}`}
                  >
                    {u.role}
                  </span>
                </TableCell>
                <TableCell className="font-mono tabular-nums text-right">
                  {fmtUSD(u.balance_usd)}
                </TableCell>
                <TableCell>
                  <AdjustBalanceButton userID={u.id} onDone={reload} />
                </TableCell>
                <TableCell>
                  <div className="flex items-center gap-1">
                    <Button
                      size="sm"
                      variant="ghost"
                      disabled={rowBusy === u.id}
                      onClick={() =>
                        patchUser(
                          u.id,
                          { role: u.role === "admin" ? "user" : "admin" },
                          t("admin.users.roleUpdated"),
                        )
                      }
                    >
                      {u.role === "admin" ? t("admin.users.makeUser") : t("admin.users.makeAdmin")}
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="text-destructive"
                      disabled={rowBusy === u.id}
                      onClick={() =>
                        patchUser(
                          u.id,
                          { disabled: !u.disabled },
                          u.disabled ? t("admin.users.enabled") : t("admin.users.disabled"),
                        )
                      }
                    >
                      {u.disabled ? t("admin.users.enable") : t("admin.users.disable")}
                    </Button>
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        <Pager
          offset={offset}
          limit={USERS_PAGE}
          total={total}
          count={users.length}
          busy={busy}
          onChange={setOffset}
          className="border-t border-border px-4 py-3"
        />
      </GlassPanel>
    </FadeIn>
  );
}

function AdjustBalanceButton({ userID, onDone }: { userID: number; onDone: () => void }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [delta, setDelta] = useState("");
  const [note, setNote] = useState("");
  return (
    <>
      <Button size="sm" variant="outline" onClick={() => setOpen(true)}>
        {t("admin.users.adjust")}
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="sm:max-w-[400px]">
          <DialogHeader>
            <DialogTitle>{t("admin.users.adjustTitle")}</DialogTitle>
          </DialogHeader>
          <div className="space-y-3 py-2">
            <div className="space-y-2">
              <Label>{t("admin.users.delta")}</Label>
              <Input
                type="number"
                step="0.01"
                placeholder={t("admin.users.deltaPlaceholder")}
                value={delta}
                onChange={(e) => setDelta(e.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label>{t("admin.users.note")}</Label>
              <Input
                placeholder={t("admin.users.notePlaceholder")}
                value={note}
                onChange={(e) => setNote(e.target.value)}
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>
              {t("common.cancel")}
            </Button>
            <Button
              onClick={async () => {
                await apiPost(`/admin/users/${userID}/balance`, {
                  delta_usd: parseFloat(delta),
                  note,
                });
                toast.success(t("admin.users.balanceUpdated"));
                onDone();
                setOpen(false);
                setDelta("");
                setNote("");
              }}
            >
              {t("admin.users.apply")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}

const ORDERS_PAGE = 50;

function PaymentsTab() {
  const { t } = useTranslation();
  const [orders, setOrders] = useState<AdminOrder[]>([]);
  const [offset, setOffset] = useState(0);
  // total is optional until the backend ships it — the title and Pager both
  // degrade gracefully when it's undefined.
  const [total, setTotal] = useState<number | undefined>(undefined);
  const [busy, setBusy] = useState(true);
  useEffect(() => {
    let cancelled = false;
    setBusy(true);
    apiGet<{ orders: AdminOrder[]; total?: number }>(
      `/admin/orders?limit=${ORDERS_PAGE}&offset=${offset}`,
    )
      .then((r) => {
        if (cancelled) return;
        setOrders(r.orders || []);
        setTotal(r.total);
      })
      .catch((e) => {
        if (!cancelled) toast.error(errMsg(e));
      })
      .finally(() => {
        if (!cancelled) setBusy(false);
      });
    return () => {
      cancelled = true;
    };
  }, [offset]);
  return (
    <FadeIn className="space-y-6">
      <GlassPanel
        title={t("admin.payments.heading", { n: total ?? orders.length })}
        description={t("admin.payments.sub")}
        bodyClassName="p-0"
      >
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("admin.payments.cols.order")}</TableHead>
              <TableHead>{t("admin.payments.cols.user")}</TableHead>
              <TableHead className="text-right">{t("admin.payments.cols.usd")}</TableHead>
              <TableHead>{t("admin.payments.cols.status")}</TableHead>
              <TableHead>{t("admin.payments.cols.created")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {orders.length === 0 && (
              <TableRow>
                <TableCell colSpan={5} className="py-12 text-center text-muted-foreground">
                  {busy ? t("common.loading") : t("admin.payments.empty")}
                </TableCell>
              </TableRow>
            )}
            {orders.map((o) => (
              <TableRow key={o.OutTradeNo}>
                <TableCell className="font-mono text-xs">{o.OutTradeNo}</TableCell>
                <TableCell>#{o.UserID}</TableCell>
                <TableCell className="font-mono tabular-nums text-right">
                  {fmtUSD(o.USDCredit)}
                </TableCell>
                <TableCell>
                  <span
                    className={`rounded border px-2 py-0.5 text-xs font-mono uppercase ${o.Status === "paid" ? "border-success/30 bg-success/15 text-success" : "border-warning/30 bg-warning/15 text-warning"}`}
                  >
                    {o.Status === "paid"
                      ? t("common.paid")
                      : o.Status === "pending"
                        ? t("common.pending")
                        : o.Status === "expired"
                          ? t("common.expired")
                          : o.Status}
                  </span>
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {o.CreatedAt && new Date(o.CreatedAt).toLocaleString()}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        <Pager
          offset={offset}
          limit={ORDERS_PAGE}
          total={total}
          count={orders.length}
          busy={busy}
          onChange={setOffset}
          className="border-t border-border px-4 py-3"
        />
      </GlassPanel>
      <AdjustmentsPanel />
    </FadeIn>
  );
}

const ADJUSTMENTS_PAGE = 50;

// AdjustmentsPanel surfaces wallet credits that arrived without a payment
// order — manual operator grants and channel signup bonuses (new-user
// rewards). Both are kind='adjust' server-side; a "signup_bonus:" ref marks
// the bonus, which this view badges distinctly.
function AdjustmentsPanel() {
  const { t } = useTranslation();
  const [rows, setRows] = useState<AdminAdjustment[]>([]);
  const [offset, setOffset] = useState(0);
  const [total, setTotal] = useState<number | undefined>(undefined);
  const [busy, setBusy] = useState(true);
  useEffect(() => {
    let cancelled = false;
    setBusy(true);
    apiGet<{ adjustments: AdminAdjustment[]; total?: number }>(
      `/admin/adjustments?limit=${ADJUSTMENTS_PAGE}&offset=${offset}`,
    )
      .then((r) => {
        if (cancelled) return;
        setRows(r.adjustments || []);
        setTotal(r.total);
      })
      .catch((e) => {
        if (!cancelled) toast.error(errMsg(e));
      })
      .finally(() => {
        if (!cancelled) setBusy(false);
      });
    return () => {
      cancelled = true;
    };
  }, [offset]);
  return (
    <GlassPanel
      title={t("admin.adjustments.heading", { n: total ?? rows.length })}
      description={t("admin.adjustments.sub")}
      bodyClassName="p-0"
    >
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t("admin.adjustments.cols.user")}</TableHead>
            <TableHead className="text-right">{t("admin.adjustments.cols.amount")}</TableHead>
            <TableHead>{t("admin.adjustments.cols.source")}</TableHead>
            <TableHead>{t("admin.adjustments.cols.note")}</TableHead>
            <TableHead>{t("admin.adjustments.cols.created")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.length === 0 && (
            <TableRow>
              <TableCell colSpan={5} className="py-12 text-center text-muted-foreground">
                {busy ? t("common.loading") : t("admin.adjustments.empty")}
              </TableCell>
            </TableRow>
          )}
          {rows.map((a) => {
            const isBonus = a.ref.startsWith("signup_bonus:");
            return (
              <TableRow key={a.id}>
                <TableCell className="text-xs">{a.email || `#${a.user_id}`}</TableCell>
                <TableCell
                  className={`font-mono tabular-nums text-right ${a.amount_usd >= 0 ? "text-success" : ""}`}
                >
                  {a.amount_usd >= 0 ? "+" : ""}
                  {fmtUSD(a.amount_usd)}
                </TableCell>
                <TableCell>
                  <span
                    className={`rounded border px-2 py-0.5 text-xs font-medium ${isBonus ? "border-primary/30 bg-primary/15 text-primary" : "border-border bg-muted/40 text-muted-foreground"}`}
                  >
                    {isBonus ? t("admin.adjustments.bonus") : t("admin.adjustments.manual")}
                  </span>
                </TableCell>
                <TableCell className="text-xs text-muted-foreground">{a.note || "—"}</TableCell>
                <TableCell className="text-muted-foreground">
                  {new Date(a.created_at * 1000).toLocaleString()}
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
      <Pager
        offset={offset}
        limit={ADJUSTMENTS_PAGE}
        total={total}
        count={rows.length}
        busy={busy}
        onChange={setOffset}
        className="border-t border-border px-4 py-3"
      />
    </GlassPanel>
  );
}
