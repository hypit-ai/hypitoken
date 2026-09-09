import * as DialogPrimitive from "@radix-ui/react-dialog";
import {
  KeyRound,
  LayoutDashboard,
  LifeBuoy,
  LogOut,
  Menu,
  Receipt,
  Shield,
  Terminal,
  User as UserIcon,
  Wallet,
  X,
} from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { LogoMark } from "@/components/icons/logo-mark";
import { LanguageToggle } from "@/components/language-toggle";
import { ThemeToggle } from "@/components/theme-toggle";
import { Button } from "@/components/ui/button";
import { useAuth } from "@/hooks/use-auth";
import { cn, fmtUSD } from "@/lib/utils";

// MobileMenu is the single off-canvas drawer used by every header in the
// app. Both the AppShell (authenticated) and PublicHeader (anonymous) mount
// it — the menu reads the auth state and conditionally renders the account
// + operator sections so we don't ship two near-identical components.
//
// Aesthetic: dense, mono accents, hairline dividers, primary accent ribbon
// down the left edge. Staggered fade-slide reveal driven by per-item
// animation-delay; no JS motion library, just CSS via tw-animate-css. The
// reveal is intentional — the drawer feels considered rather than slapped on.

interface NavItem {
  to: string;
  label: string;
  icon: React.ComponentType<{ className?: string }>;
  end?: boolean;
  badge?: number;
}

interface Props {
  // Which set of routes to show. "public" hides /app/* items, "app" shows
  // them after the marketing links. Operator items are gated on user.role.
  variant: "public" | "app";
  // Support-ticket unread badge, sourced from the caller's own
  // useTicketUnread() rather than a second instance in here — AppShell and
  // SiteNav each already poll it for their own sidebar/toast, and this
  // component is mounted alongside both, so a private hook here doubled
  // the GET /me/tickets/unread on every app page load.
  unread?: number;
}

export function MobileMenu({ variant, unread = 0 }: Props) {
  const [open, setOpen] = useState(false);
  const { user, signOut } = useAuth();
  const { t } = useTranslation();
  const nav = useNavigate();
  const loc = useLocation();

  const marketing: NavItem[] = [
    { to: "/", label: t("nav.home"), icon: LayoutDashboard, end: true },
    { to: "/pricing", label: t("nav.pricing"), icon: Wallet },
    { to: "/docs", label: t("nav.docs"), icon: Terminal },
    { to: "/status", label: t("nav.status"), icon: Receipt },
  ];

  const appItems: NavItem[] = [
    { to: "/app", label: t("nav.dashboard"), icon: LayoutDashboard, end: true },
    { to: "/app/tokens", label: t("nav.tokens"), icon: KeyRound },
    { to: "/app/billing", label: t("nav.billing"), icon: Wallet },
    { to: "/app/logs", label: t("nav.logs"), icon: Receipt },
    { to: "/app/console", label: t("nav.console"), icon: Terminal },
    { to: "/app/support", label: t("nav.support"), icon: LifeBuoy, badge: unread },
  ];

  const isActive = (item: NavItem) =>
    item.end ? loc.pathname === item.to : loc.pathname.startsWith(item.to);

  // Build the sections actually rendered, in order.
  type Section = { key: string; eyebrow: string; items: NavItem[] };
  const sections: Section[] = [];
  sections.push({ key: "marketing", eyebrow: t("nav.product", "Product"), items: marketing });
  // Account section shows whenever the user is logged in — even from a
  // marketing page they may want to jump straight into /app on mobile.
  if (user) {
    sections.push({ key: "account", eyebrow: t("nav.account", "Account"), items: appItems });
  }
  if (user?.role === "admin") {
    sections.push({
      key: "operator",
      eyebrow: t("nav.operator", "Operator"),
      items: [{ to: "/app/admin", label: t("nav.admin", "Operator"), icon: Shield }],
    });
  }

  // Flatten so we can compute a global delay index for the cascade reveal.
  let cascadeIdx = 0;

  return (
    <DialogPrimitive.Root open={open} onOpenChange={setOpen}>
      <DialogPrimitive.Trigger asChild>
        <button
          type="button"
          aria-label="Open menu"
          className="inline-flex h-9 w-9 items-center justify-center rounded-md border border-border-strong/60 bg-card text-foreground transition-colors hover:bg-accent lg:hidden"
        >
          <Menu className="h-4 w-4" />
        </button>
      </DialogPrimitive.Trigger>

      <DialogPrimitive.Portal>
        {/* Backdrop. fade + a hair of blur so the page reads "secondary"
            without losing context. */}
        <DialogPrimitive.Overlay
          className={cn(
            "fixed inset-0 z-50 bg-black/55 backdrop-blur-[2px]",
            "data-[state=open]:animate-in data-[state=open]:fade-in-0",
            "data-[state=closed]:animate-out data-[state=closed]:fade-out-0",
            "duration-200",
          )}
        />

        {/* Sheet. slide in from the right with a tiny scale assist —
            "deliberate" not "frantic". */}
        <DialogPrimitive.Content
          aria-describedby={undefined}
          className={cn(
            "fixed right-0 top-0 z-50 flex h-dvh w-[85vw] max-w-[340px] flex-col",
            "border-l border-border-strong/80 bg-background",
            "shadow-[-12px_0_48px_-12px_rgba(0,0,0,0.35)]",
            "data-[state=open]:animate-in data-[state=open]:slide-in-from-right",
            "data-[state=closed]:animate-out data-[state=closed]:slide-out-to-right",
            "duration-300 ease-out",
          )}
        >
          {/* Vertical accent ribbon — subtle, full-height. */}
          <div
            aria-hidden
            className="pointer-events-none absolute inset-y-0 left-0 w-px bg-gradient-to-b from-primary/0 via-primary/40 to-primary/0"
          />

          {/* Header — brand mark, balance pill (if logged in), close. */}
          <header className="flex items-center justify-between gap-3 border-b border-border/80 px-5 py-4">
            <Link
              to={variant === "app" ? "/app" : "/"}
              onClick={() => setOpen(false)}
              className="flex items-center gap-2 font-display text-lg font-semibold tracking-tight"
            >
              <LogoMark />
              HypiToken
            </Link>
            <DialogPrimitive.Close asChild>
              <button
                type="button"
                aria-label="Close menu"
                className="inline-flex h-8 w-8 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
              >
                <X className="h-4 w-4" />
              </button>
            </DialogPrimitive.Close>
          </header>

          {/* Balance strip — only when signed in. */}
          {user && (
            <div
              className="border-b border-border/60 bg-muted/30 px-5 py-3 animate-in fade-in-0 slide-in-from-top-1 fill-mode-both"
              style={{ animationDelay: "60ms", animationDuration: "320ms" }}
            >
              <div className="font-mono text-[10px] uppercase tracking-[0.15em] text-muted-foreground">
                {t("common.balance")}
              </div>
              <div className="mt-0.5 flex items-baseline justify-between gap-3">
                <span className="font-mono text-2xl font-semibold tabular-nums">
                  {fmtUSD(user.balance_usd)}
                </span>
                <span className="truncate font-mono text-[11px] text-muted-foreground">
                  {user.email}
                </span>
              </div>
            </div>
          )}

          {/* Sections */}
          <nav className="flex-1 overflow-y-auto px-3 py-4">
            {sections.map((section) => (
              <div key={section.key} className="mb-6 last:mb-0">
                <div className="px-3 pb-2 font-mono text-[10px] uppercase tracking-[0.15em] text-muted-foreground/80">
                  <span className="inline-flex items-center gap-1.5">
                    <span className="h-px w-3 bg-muted-foreground/40" />
                    {section.eyebrow}
                  </span>
                </div>
                <ul className="flex flex-col gap-0.5">
                  {section.items.map((item) => {
                    const active = isActive(item);
                    const delay = 80 + cascadeIdx++ * 35;
                    return (
                      <li
                        key={item.to}
                        className="animate-in fade-in-0 slide-in-from-right-2 fill-mode-both"
                        style={{ animationDelay: `${delay}ms`, animationDuration: "320ms" }}
                      >
                        <Link
                          to={item.to}
                          onClick={() => setOpen(false)}
                          className={cn(
                            "group relative flex items-center gap-3 rounded-md px-3 py-2.5 text-sm transition-all",
                            active
                              ? "bg-primary/10 text-primary"
                              : "text-foreground/80 hover:bg-accent hover:text-foreground",
                          )}
                        >
                          {active && (
                            <span
                              aria-hidden
                              className="absolute inset-y-1 left-0 w-[2px] rounded-full bg-primary"
                            />
                          )}
                          <item.icon
                            className={cn(
                              "h-4 w-4 shrink-0 transition-transform group-hover:-translate-y-px",
                              active && "text-primary",
                            )}
                          />
                          <span className="flex-1 truncate">{item.label}</span>
                          {item.badge ? (
                            <span className="rounded-full bg-primary px-1.5 py-0.5 text-[10px] font-semibold leading-none tabular-nums text-primary-foreground">
                              {item.badge > 99 ? "99+" : item.badge}
                            </span>
                          ) : null}
                          {active && (
                            <span className="font-mono text-[10px] uppercase tracking-wider text-primary/70">
                              ·
                            </span>
                          )}
                        </Link>
                      </li>
                    );
                  })}
                </ul>
              </div>
            ))}
          </nav>

          {/* Footer — controls + auth actions. */}
          <footer
            className="border-t border-border/80 px-4 py-3 animate-in fade-in-0 slide-in-from-bottom-2 fill-mode-both"
            style={{ animationDelay: `${80 + cascadeIdx * 35 + 40}ms`, animationDuration: "320ms" }}
          >
            <div className="mb-3 flex items-center gap-2">
              <LanguageToggle />
              <ThemeToggle />
              <span className="flex-1" />
              {user ? (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    setOpen(false);
                    signOut();
                    nav("/login");
                  }}
                  className="gap-1.5"
                >
                  <LogOut className="h-3.5 w-3.5" /> {t("nav.signOut")}
                </Button>
              ) : (
                <Button
                  size="sm"
                  onClick={() => {
                    setOpen(false);
                    nav("/login");
                  }}
                  className="gap-1.5"
                >
                  <UserIcon className="h-3.5 w-3.5" /> {t("nav.signIn")}
                </Button>
              )}
            </div>
            <p className="font-mono text-[10px] uppercase tracking-[0.15em] text-muted-foreground/60">
              HypiToken · {new Date().getFullYear()}
            </p>
          </footer>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}
