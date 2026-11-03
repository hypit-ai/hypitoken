// ─────────────────────────────────────────────────────────────────────────
// Shared client-onboarding panel. Self-contained: depends only on
// @/components/ui/{button,tabs}, @/lib/utils, ./onboarding-links and
// lucide-react — all present in BOTH sibling gateways — so it can be kept
// BYTE-IDENTICAL across hypitoken + CPA-Claude. All user-facing copy comes
// through `labels` (defaults to English); hypitoken passes i18n strings,
// CPA-Claude uses the defaults. If you edit one copy, sync the other.
// ─────────────────────────────────────────────────────────────────────────
import { ArrowRightLeft, Check, Copy, MessageSquare, Terminal } from "lucide-react";
import { useId, useState } from "react";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Button } from "@/components/ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  buildCCSwitchURL,
  CC_SWITCH_DEFAULT_MODELS,
  buildCherryStudioURL,
  claudeCodeConfig,
  claudeCodeEnv,
  claudeCodeInstall,
  codexConfig,
  codexInstall,
  type OnboardingConfig,
  type OnboardingOS,
  openDeepLink,
} from "@/lib/onboarding-links";
import { cn, copyToClipboard } from "@/lib/utils";

export interface OnboardingLabels {
  oneClick: string;
  oneClickHint: string;
  openInCCSwitch: string;
  ccNothingHappened: string;
  ccNothingHappenedHint: string;
  ccCopyLink: string;
  ccGetApp: string;
  primaryModel: string;
  orManual: string;
  desktopTitle: string;
  desktopHint: string;
  desktopGuide: string;
  cliOptional: string;
  step1Install: string;
  step2Config: string;
  step2Env: string;
  step2EnvHint: string;
  step2File: string;
  step2FileHint: string;
  step3Run: string;
  cherryTitle: string;
  cherryHint: string;
  importToCherry: string;
  yourToken: string;
  copy: string;
  copied: string;
}

const DEFAULT_LABELS: OnboardingLabels = {
  oneClick: "One-click setup",
  oneClickHint:
    "CC Switch writes the client config for you — no manual file editing. Requires the CC Switch desktop app.",
  openInCCSwitch: "Open in CC Switch",
  ccNothingHappened: "Nothing happened?",
  ccNothingHappenedHint:
    'The browser hands the link to the CC Switch app; it cannot tell whether the app received it. If nothing opened, copy the link and paste it into CC Switch, or install the app first. On Linux, a package-provided "CC Switch.desktop" whose Exec line lacks %u will swallow the URL — prefer cc-switch-handler.desktop.',
  ccCopyLink: "Copy import link",
  ccGetApp: "Get CC Switch",
  primaryModel: "Primary model",
  orManual: "or configure manually",
  desktopTitle: "Codex Desktop App",
  desktopHint:
    "Use CC Switch Routing mode: import the token, open Codex → Routing, click Start routing, and route to HypiToken. Preserve ChatGPT sign-in, then fully quit and reopen Codex.",
  desktopGuide: "Desktop setup guide",
  cliOptional: "Using Codex CLI? Show terminal setup",
  step1Install: "1. Install the CLI",
  step2Config: "2. Configure",
  step2Env: "Environment variables (this shell)",
  step2EnvHint: "Paste into your terminal, then run claude in the same session.",
  step2File: "Or persist to the config file",
  step2FileHint: "Survives new terminals.",
  step3Run: "3. Run it",
  cherryTitle: "Chat client",
  cherryHint: "Import this gateway into Cherry Studio as an OpenAI-compatible provider.",
  importToCherry: "Import to Cherry Studio",
  yourToken: "Your token",
  copy: "Copy",
  copied: "Copied",
};

function CodeBlock({
  code,
  copyLabel,
  copiedLabel,
}: {
  code: string;
  copyLabel: string;
  copiedLabel: string;
}) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="relative mt-2 w-full min-w-0 rounded-lg border border-border bg-[#0d1117]">
      <button
        type="button"
        onClick={async () => {
          await copyToClipboard(code);
          setCopied(true);
          setTimeout(() => setCopied(false), 1500);
        }}
        className="absolute right-2 top-2 z-10 inline-flex items-center gap-1 rounded px-2 py-0.5 text-xs text-zinc-400 transition-colors hover:bg-white/10 hover:text-white"
      >
        {copied ? <Check className="h-3 w-3 text-green-400" /> : <Copy className="h-3 w-3" />}
        {copied ? copiedLabel : copyLabel}
      </button>
      <pre className="overflow-x-auto px-4 py-4 pr-16 font-mono text-xs leading-relaxed text-zinc-200">
        {code}
      </pre>
    </div>
  );
}

function OSPills({ os, setOS }: { os: OnboardingOS; setOS: (o: OnboardingOS) => void }) {
  return (
    <div className="flex gap-2 pt-1">
      {(["macOS", "Windows", "Linux"] as const).map((s) => (
        <button
          key={s}
          type="button"
          onClick={() => setOS(s)}
          className={cn(
            "rounded-full border px-3 py-1 text-xs font-medium transition-colors",
            os === s
              ? "border-primary bg-primary/10 text-primary"
              : "border-border text-muted-foreground hover:border-foreground/30 hover:text-foreground",
          )}
        >
          {s}
        </button>
      ))}
    </div>
  );
}

/**
 * One-click CC Switch import.
 *
 * A custom-scheme navigation is fire-and-forget: the page cannot observe
 * whether the OS found a handler, whether the app was installed, or whether it
 * accepted the payload. So the click reveals a fallback instead of assuming
 * success — that "click does nothing, with no explanation" was the entire
 * failure mode this replaces.
 */
function CCSwitchCard({
  app,
  config,
  L,
}: {
  app: "claude" | "codex";
  config: OnboardingConfig;
  L: OnboardingLabels;
}) {
  const [tried, setTried] = useState(false);
  const [copied, setCopied] = useState(false);
  const modelId = useId();
  const [model, setModel] = useState<string>(CC_SWITCH_DEFAULT_MODELS[app].model);
  const url = buildCCSwitchURL(config, app, { model: model.trim() });

  return (
    <div className="rounded-md border border-primary/30 bg-primary/5 p-3">
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="text-sm font-medium text-foreground">{L.oneClick}</div>
          <div className="mt-0.5 text-xs text-muted-foreground">{L.oneClickHint}</div>
        </div>
        <Button asChild size="sm" className="shrink-0 gap-1.5">
          <a
            href={model.trim() ? url : undefined}
            aria-disabled={!model.trim()}
            onClick={(event) => {
              if (!model.trim()) event.preventDefault();
              else setTried(true);
            }}
          >
            <ArrowRightLeft data-icon="inline-start" />
            {L.openInCCSwitch}
          </a>
        </Button>
      </div>

      <div className="mt-3 flex flex-col gap-2">
        <Label htmlFor={modelId}>{L.primaryModel}</Label>
        <Input
          id={modelId}
          value={model}
          onChange={(event) => setModel(event.target.value)}
          required
        />
        <a
          href="https://ccswitch.io/"
          target="_blank"
          rel="noreferrer noopener"
          className="text-xs text-primary underline underline-offset-2"
        >
          {L.ccGetApp}
        </a>
      </div>

      <div className="mt-3 flex flex-col gap-2">
        <Input
          aria-label={L.ccCopyLink}
          value={url}
          readOnly
          onFocus={(event) => event.target.select()}
        />
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={!model.trim()}
          onClick={async () => {
            await copyToClipboard(url);
            setCopied(true);
            setTimeout(() => setCopied(false), 1500);
          }}
        >
          {copied ? <Check data-icon="inline-start" /> : <Copy data-icon="inline-start" />}
          {copied ? L.copied : L.ccCopyLink}
        </Button>
      </div>
      {tried && (
        <div className="mt-3 border-t border-primary/20 pt-3">
          <div className="text-xs font-medium text-foreground">{L.ccNothingHappened}</div>
          <p className="mt-1 text-xs text-muted-foreground">{L.ccNothingHappenedHint}</p>
        </div>
      )}
    </div>
  );
}

export interface ClientOnboardingProps {
  config: OnboardingConfig;
  labels?: Partial<OnboardingLabels>;
  /** Native Windows is the default; callers can select another OS. */
  initialOS?: OnboardingOS;
}

export function ClientOnboarding({ config, labels, initialOS }: ClientOnboardingProps) {
  const L = { ...DEFAULT_LABELS, ...labels };
  const [os, setOS] = useState<OnboardingOS>(initialOS ?? "Windows");

  const install = {
    install: claudeCodeInstall(os),
    env: claudeCodeEnv(config, os),
    config: claudeCodeConfig(config, os),
  };
  const codex = { install: codexInstall(os), config: codexConfig(config, os) };
  const cb = (code: string) => <CodeBlock code={code} copyLabel={L.copy} copiedLabel={L.copied} />;

  const ccSwitchButton = (app: "claude" | "codex") => (
    <CCSwitchCard app={app} config={config} L={L} />
  );

  const manualDivider = (
    <div className="flex items-center gap-3 pt-1 text-[11px] uppercase tracking-wider text-muted-foreground">
      <span className="h-px flex-1 bg-border" />
      {L.orManual}
      <span className="h-px flex-1 bg-border" />
    </div>
  );

  return (
    <div className="space-y-3">
      <OSPills os={os} setOS={setOS} />

      <Tabs defaultValue="codex" className="mt-1 min-w-0">
        <TabsList className="w-full">
          <TabsTrigger value="codex" className="flex-1 gap-1.5">
            <Terminal className="h-3.5 w-3.5" /> Codex
          </TabsTrigger>
          <TabsTrigger value="claude-code" className="flex-1 gap-1.5">
            <Terminal className="h-3.5 w-3.5" /> Claude Code
          </TabsTrigger>
        </TabsList>

        <TabsContent value="claude-code" className="space-y-3 pt-2 min-w-0">
          {ccSwitchButton("claude")}
          {manualDivider}
          <div className="min-w-0">
            <p className="text-sm font-medium text-muted-foreground">{L.step1Install}</p>
            {cb(install.install)}
          </div>
          <div className="min-w-0">
            <p className="text-sm font-medium text-muted-foreground">{L.step2Config}</p>
            <p className="mt-2 text-xs font-medium text-foreground">{L.step2Env}</p>
            <p className="text-[11px] text-muted-foreground">{L.step2EnvHint}</p>
            {cb(install.env)}
            <p className="mt-3 text-xs font-medium text-foreground">{L.step2File}</p>
            <p className="text-[11px] text-muted-foreground">{L.step2FileHint}</p>
            {cb(install.config)}
          </div>
          <div className="min-w-0">
            <p className="text-sm font-medium text-muted-foreground">{L.step3Run}</p>
            {cb("claude")}
          </div>
        </TabsContent>

        <TabsContent value="codex" className="space-y-3 pt-2 min-w-0">
          <div className="space-y-2 text-sm">
            <p className="font-medium">{L.desktopTitle}</p>
            <p className="text-muted-foreground">{L.desktopHint}</p>
            <a href="/docs/codex-cli" className="text-primary underline underline-offset-2">
              {L.desktopGuide}
            </a>
          </div>
          {ccSwitchButton("codex")}
          <details className="space-y-3">
            <summary className="cursor-pointer text-sm text-muted-foreground">
              {L.cliOptional}
            </summary>
            <div className="min-w-0">
              <p className="text-sm font-medium text-muted-foreground">{L.step1Install}</p>
              {cb(codex.install)}
            </div>
            <div className="min-w-0">
              <p className="text-sm font-medium text-muted-foreground">{L.step2Config}</p>
              {cb(codex.config)}
            </div>
            <div className="min-w-0">
              <p className="text-sm font-medium text-muted-foreground">{L.step3Run}</p>
              {cb("codex")}
            </div>
          </details>
        </TabsContent>
      </Tabs>

      <div className="rounded-md border border-border bg-muted/30 p-3">
        <div className="flex items-center justify-between gap-3">
          <div className="min-w-0">
            <div className="flex items-center gap-1.5 text-sm font-medium text-foreground">
              <MessageSquare className="h-3.5 w-3.5" /> {L.cherryTitle}
            </div>
            <div className="mt-0.5 text-xs text-muted-foreground">{L.cherryHint}</div>
          </div>
          <Button
            type="button"
            size="sm"
            variant="outline"
            className="shrink-0 gap-1.5"
            onClick={() => openDeepLink(buildCherryStudioURL(config))}
          >
            <ArrowRightLeft className="h-3.5 w-3.5" />
            {L.importToCherry}
          </Button>
        </div>
      </div>

      <div className="min-w-0 rounded-md border border-border bg-muted/30 p-3 text-xs text-muted-foreground">
        <div className="font-medium text-foreground">{L.yourToken}</div>
        <code className="mt-1 block w-full overflow-x-auto rounded bg-muted px-1.5 py-0.5 font-mono">
          {config.token}
        </code>
      </div>
    </div>
  );
}
