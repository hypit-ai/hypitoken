import {
  Building2,
  ChartColumnBig,
  KeyRound,
  LayoutDashboard,
  LifeBuoy,
  LogOut,
  Receipt,
  Shield,
  Terminal,
  Trophy,
  User as UserIcon,
  Wallet,
} from "lucide-react";
import { motion, useReducedMotion } from "motion/react";
import { useTranslation } from "react-i18next";
import { Link, NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { DiscordIcon } from "@/components/icons/discord";
import { LogoMark } from "@/components/icons/logo-mark";
import { LanguageToggle } from "@/components/language-toggle";
import { MobileMenu } from "@/components/layout/mobile-menu";
import { ThemeToggle } from "@/components/theme-toggle";
import { Button } from "@/components/ui/button";
import { useAuth } from "@/hooks/use-auth";
import { useTicketUnread } from "@/hooks/use-ticket-unread";
import { DISCORD_INVITE } from "@/lib/social";
import { cn, fmtUSD } from "@/lib/utils";

const DISCORD_BLURPLE = "#5865f2";

// NAV_ITEMS is keyed by i18n string id rather than the literal label —
// resolved at render time so language switching reflows the sidebar.
const NAV_ITEMS = [
  { to: "/app", labelKey: "nav.dashboard", icon: LayoutDashboard, end: true },
  { to: "/app/leaderboard", labelKey: "nav.arena", icon: Trophy },
  { to: "/app/tokens", labelKey: "nav.tokens", icon: KeyRound },
  { to: "/app/billing", labelKey: "nav.billing", icon: Wallet },
  { to: "/app/usage", labelKey: "nav.usage", icon: ChartColumnBig },
  { to: "/app/logs", labelKey: "nav.logs", icon: Receipt },
  { to: "/app/console", labelKey: "nav.console", icon: Terminal },
  { to: "/app/support", labelKey: "nav.support", icon: LifeBuoy },
];

// SideNavLink — a sidebar row that, when active, renders a shared glass pill
// + a left primary accent bar. The pill uses a motion layoutId so it slides
// between rows as you navigate, the way a polished product sidebar does.
function SideNavLink({
  to,
  end,
  icon: Icon,
  label,
  badge,
}: {
  to: string;
  end?: boolean;
  icon: typeof Wallet;
  label: string;
  badge?: number;
}) {
  const { pathname } = useLocation();
  const active = end ? pathname === to : pathname === to || pathname.startsWith(`${to}/`);
  return (
    <NavLink
      to={to}
      end={end}
      className={cn(
        "relative flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm transition-colors",
        active ? "font-medium text-primary" : "text-muted-foreground hover:text-foreground",
      )}
    >
      {active && (
        <motion.span
          layoutId="app-nav-pill"
          className="glass absolute inset-0 -z-10 rounded-lg"
          transition={{ type: "spring", stiffness: 380, damping: 32 }}
        >
          <span className="absolute left-0 top-1/2 h-5 w-[3px] -translate-y-1/2 rounded-full bg-primary" />
        </motion.span>
      )}
      <Icon className="h-4 w-4" />
      {label}
      {badge ? (
        <span className="ml-auto rounded-full bg-primary px-1.5 py-0.5 text-[10px] font-semibold leading-none tabular-nums text-primary-foreground">
          {badge > 99 ? "99+" : badge}
        </span>
      ) : null}
    </NavLink>
  );
}

export function AppShell() {
  const { user } = useAuth();
  const { t } = useTranslation();
  // Single prompt-enabled instance: only the shell may raise the entry toast.
  const { unread } = useTicketUnread({ prompt: true });
  return (
    <div className="relative min-h-dvh bg-background text-foreground">
      {/* faint ambient gradient mesh — frames the whole app without competing
          with content. Fixed so it stays put while the page scrolls. */}
      <div
        aria-hidden
        className="pointer-events-none fixed inset-0 -z-10"
        style={{
          background:
            "radial-gradient(50% 40% at 12% 0%, color-mix(in oklch, var(--primary) 9%, transparent), transparent 70%)," +
            "radial-gradient(45% 45% at 100% 8%, color-mix(in oklch, var(--info, var(--primary)) 7%, transparent), transparent 72%)",
        }}
      />
      <Header unread={unread} />
      <div className="mx-auto flex max-w-7xl gap-6 px-4 py-6 md:px-6 lg:py-10">
        <aside className="hidden w-56 flex-shrink-0 lg:block">
          <nav className="sticky top-24 flex flex-col gap-1">
            {NAV_ITEMS.map((n) => (
              <SideNavLink
                key={n.to}
                to={n.to}
                end={n.end}
                icon={n.icon}
                label={t(n.labelKey)}
                badge={n.to === "/app/support" ? unread : undefined}
              />
            ))}
            {user?.workspaces?.some((w) => w.type === "enterprise" && w.role === "admin") && (
              <SideNavLink to="/app/workspace" icon={Building2} label={t("nav.workspace")} />
            )}
            {user?.role === "admin" && (
              <>
                <div className="mt-6 mb-1 px-3 text-xs uppercase tracking-wider text-muted-foreground">
                  {t("nav.operator")}
                </div>
                <SideNavLink to="/app/admin" icon={Shield} label={t("nav.admin")} />
              </>
            )}
          </nav>
        </aside>
        <main className="min-w-0 flex-1">
          <Outlet />
        </main>
      </div>
      <FloatingDiscord />
    </div>
  );
}

// FloatingDiscord — an upper-right FAB linking to the community server. Lives in
// AppShell so it persists across every signed-in route. Sits just below the
// sticky header pill (top-0, z-40) so it never overlaps the nav controls; z-30
// keeps it under the header and dialog overlays (z-50). The label pill expands
// on hover/focus.
function FloatingDiscord() {
  const { t } = useTranslation();
  const reduce = useReducedMotion();
  return (
    <motion.a
      href={DISCORD_INVITE}
      target="_blank"
      rel="noopener noreferrer"
      aria-label={t("common.joinDiscord")}
      title={t("common.joinDiscord")}
      className="group fixed top-20 right-5 z-30 flex items-center gap-0 rounded-full text-white shadow-lg ring-1 ring-white/10 md:top-24 md:right-6"
      style={{ backgroundColor: DISCORD_BLURPLE, boxShadow: `0 10px 30px -8px ${DISCORD_BLURPLE}` }}
      initial={reduce ? false : { opacity: 0, y: -16, scale: 0.9 }}
      animate={reduce ? undefined : { opacity: 1, y: 0, scale: 1 }}
      transition={reduce ? undefined : { delay: 0.4, duration: 0.4, ease: [0.22, 1, 0.36, 1] }}
      whileHover={reduce ? undefined : { scale: 1.05 }}
      whileTap={reduce ? undefined : { scale: 0.95 }}
    >
      <span className="grid size-12 place-items-center">
        <DiscordIcon className="size-6" />
      </span>
      <span className="max-w-0 overflow-hidden whitespace-nowrap text-sm font-medium opacity-0 transition-all duration-300 group-hover:max-w-[10rem] group-hover:pr-5 group-hover:opacity-100 group-focus-visible:max-w-[10rem] group-focus-visible:pr-5 group-focus-visible:opacity-100">
        {t("common.joinDiscord")}
      </span>
    </motion.a>
  );
}

// unread is threaded down from AppShell's single useTicketUnread() call —
// see the MobileMenu prop comment for why this can't fetch its own copy.
function Header({ unread }: { unread: number }) {
  const { user, signOut } = useAuth();
  const nav = useNavigate();
  const { t } = useTranslation();
  return (
    <div className="sticky top-0 z-40 px-4 pt-3 md:px-6">
      <header className="glass mx-auto flex max-w-7xl items-center justify-between gap-4 rounded-full px-3 py-2 md:px-4">
        <div className="flex items-center gap-5">
          <Link
            to="/app"
            viewTransition
            className="flex items-center gap-2 pl-1 font-display text-lg font-semibold tracking-tight"
          >
            <LogoMark />
            HypiToken
          </Link>
          <nav className="hidden items-center gap-1 md:flex">
            <Link
              to="/"
              viewTransition
              className="rounded-full px-3 py-1.5 text-sm text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
            >
              {t("nav.home")}
            </Link>
            <Link
              to="/pricing"
              viewTransition
              className="rounded-full px-3 py-1.5 text-sm text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
            >
              {t("nav.pricing")}
            </Link>
            <Link
              to="/docs"
              viewTransition
              className="rounded-full px-3 py-1.5 text-sm text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
            >
              {t("nav.docs")}
            </Link>
            <Link
              to="/status"
              viewTransition
              className="rounded-full px-3 py-1.5 text-sm text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
            >
              {t("nav.status")}
            </Link>
          </nav>
        </div>
        <div className="flex items-center gap-2">
          {user && (
            <Link
              to="/app/billing"
              className="glass hidden items-center gap-2.5 rounded-full px-3.5 py-1.5 transition-shadow hover:shadow-md sm:flex"
            >
              <Wallet className="h-3.5 w-3.5 text-primary" />
              <span className="text-xs uppercase text-muted-foreground tracking-wider">
                {t("common.balance")}
              </span>
              <span className="font-mono text-sm font-semibold tabular-nums">
                {fmtUSD(user.balance_usd)}
              </span>
            </Link>
          )}
          <div className="hidden items-center gap-2 lg:flex">
            <LanguageToggle />
            <ThemeToggle />
            {user ? (
              <Button
                variant="outline"
                size="sm"
                onClick={() => {
                  signOut();
                  nav("/login");
                }}
                className="gap-1.5"
              >
                <LogOut className="h-3.5 w-3.5" />
                <span className="hidden sm:inline">{t("nav.signOut")}</span>
              </Button>
            ) : (
              <Button size="sm" onClick={() => nav("/login")}>
                <UserIcon className="h-3.5 w-3.5 mr-1.5" /> {t("nav.signIn")}
              </Button>
            )}
          </div>
          <MobileMenu variant="app" unread={unread} />
        </div>
      </header>
    </div>
  );
}

// SiteNav — the canonical public navigation pill (logo + section links +
// language/theme toggles + auth CTAs). Shared *verbatim* between the public
// pages (via PublicLayout/PublicHeader) and the homepage's scroll-aware
// FloatingNav so the bar is pixel-identical everywhere — this is the single
// source of truth for "the homepage nav style". Every link opts into the View
// Transitions API (viewTransition) so route changes cross-fade rather than
// hard-cutting.
export function SiteNav() {
  const { user } = useAuth();
  const { t } = useTranslation();
  // No prompt here — the entry toast belongs to AppShell alone. This still
  // needs its own poll (SiteNav never mounts alongside AppShell) so the
  // drawer's support badge works for a signed-in user browsing marketing
  // pages; passing it down avoids yet another private hook inside MobileMenu.
  const { unread } = useTicketUnread();
  const linkCls = ({ isActive }: { isActive: boolean }) =>
    cn(
      "rounded-full px-3 py-1.5 text-sm transition-colors",
      isActive
        ? "bg-accent text-foreground"
        : "text-muted-foreground hover:bg-accent hover:text-foreground",
    );
  return (
    <header className="glass mx-auto flex max-w-6xl items-center justify-between gap-4 rounded-full px-3 py-2 md:px-4">
      <Link
        to="/"
        viewTransition
        className="flex items-center gap-2 pl-1 font-display text-lg font-semibold tracking-tight"
      >
        <LogoMark />
        HypiToken
      </Link>
      <nav className="hidden items-center gap-1 lg:flex">
        <NavLink to="/" end viewTransition className={linkCls}>
          {t("nav.home")}
        </NavLink>
        <NavLink to="/pricing" viewTransition className={linkCls}>
          {t("nav.pricing")}
        </NavLink>
        <NavLink to="/docs" viewTransition className={linkCls}>
          {t("nav.docs")}
        </NavLink>
        <NavLink to="/status" viewTransition className={linkCls}>
          {t("nav.status")}
        </NavLink>
      </nav>
      <div className="flex items-center gap-2">
        <div className="hidden items-center gap-1.5 lg:flex">
          <LanguageToggle />
          <ThemeToggle />
          {user ? (
            <Button asChild size="sm" className="rounded-full">
              <Link to="/app" viewTransition>
                {t("nav.dashboard")} →
              </Link>
            </Button>
          ) : (
            <>
              <Button asChild variant="ghost" size="sm" className="rounded-full">
                <Link to="/login" viewTransition>
                  {t("nav.signIn")}
                </Link>
              </Button>
              <Button asChild size="sm" className="rounded-full">
                <Link to="/register" viewTransition>
                  {t("nav.signUp")}
                </Link>
              </Button>
            </>
          )}
        </div>
        <MobileMenu variant="public" unread={unread} />
      </div>
    </header>
  );
}

// PublicHeader — the sticky glass-pill bar wrapping SiteNav. Rendered once by
// PublicLayout so it *persists* across public route changes (the header DOM
// node is never unmounted/remounted), which is what removes the navbar flicker
// when navigating pricing ↔ docs ↔ status.
export function PublicHeader() {
  return (
    <div className="sticky top-0 z-40 px-4 pt-3 md:px-6">
      <SiteNav />
    </div>
  );
}

// PublicLayout — shared chrome for every public marketing page. Owns the
// background + persistent header; child routes render only their content into
// the <Outlet/>. Because the header lives here (not inside each page), it stays
// mounted across navigations — no remount, no flicker, one consistent nav.
export function PublicLayout() {
  return (
    <div className="min-h-dvh bg-background text-foreground">
      <PublicHeader />
      <Outlet />
    </div>
  );
}
