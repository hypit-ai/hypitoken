import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { apiPatch } from "@/lib/api";
import type { Credential } from "@/lib/types";
import { errMsg } from "@/lib/utils";
import { AllowedModelsField, parseAllowedModels } from "./allowed-models-field";

export function EditCredentialDialog({
  cred,
  onClose,
  onSaved,
}: {
  cred: Credential | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const [label, setLabel] = useState("");
  const [group, setGroup] = useState("");
  const [proxy, setProxy] = useState("");
  const [base, setBase] = useState("");
  const [apiKey, setAPIKey] = useState("");
  const [maxC, setMaxC] = useState("");
  const [disabled, setDisabled] = useState(false);
  const [modelMap, setModelMap] = useState("");
  const [allowedModels, setAllowedModels] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (cred) {
      setLabel(cred.label || "");
      setGroup(cred.group || "");
      setProxy(cred.proxy_url || "");
      setBase(cred.base_url || "");
      setAPIKey("");
      setMaxC(String(cred.max_concurrent ?? 0));
      setDisabled(!!cred.disabled);
      setAllowedModels((cred.allowed_models ?? []).join("\n"));
      setModelMap(
        cred.model_map && Object.keys(cred.model_map).length > 0
          ? JSON.stringify(cred.model_map, null, 2)
          : "",
      );
    }
  }, [cred]);

  if (!cred) return null;
  const isAPIKey = cred.kind === "apikey";
  const save = async () => {
    setBusy(true);
    try {
      const body: {
        label: string;
        group: string;
        proxy_url: string;
        max_concurrent: number;
        disabled: boolean;
        base_url?: string;
        api_key?: string;
        model_map?: Record<string, string>;
        allowed_models?: string[];
      } = {
        label,
        group,
        proxy_url: proxy,
        max_concurrent: Number(maxC) || 0,
        disabled,
      };
      if (isAPIKey) {
        body.base_url = base;
        body.allowed_models = parseAllowedModels(allowedModels);
        if (apiKey.trim() !== "") {
          body.api_key = apiKey.trim();
        }
      }
      if (modelMap.trim() === "") {
        body.model_map = {};
      } else {
        try {
          body.model_map = JSON.parse(modelMap);
        } catch (e) {
          toast.error(t("admin.creds.modelMapParseError", { msg: errMsg(e) }));
          setBusy(false);
          return;
        }
      }
      await apiPatch(`/admin/credentials/${encodeURIComponent(cred.id)}`, body);
      toast.success(t("admin.creds.toast.saved"));
      onSaved();
      onClose();
    } catch (e) {
      toast.error(errMsg(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={!!cred} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-[560px]">
        <DialogHeader>
          <DialogTitle>{t("admin.creds.edit.title", { label: cred.label })}</DialogTitle>
          <DialogDescription className="font-mono text-xs">{cred.id}</DialogDescription>
        </DialogHeader>
        <div className="grid gap-3 py-2">
          <div className="space-y-2">
            <Label htmlFor="ec-label">{t("admin.creds.cols.label")}</Label>
            <Input id="ec-label" value={label} onChange={(e) => setLabel(e.target.value)} />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-2">
              <Label htmlFor="ec-group">{t("admin.creds.cols.group")}</Label>
              <Input
                id="ec-group"
                value={group}
                onChange={(e) => setGroup(e.target.value)}
                placeholder={t("admin.creds.edit.groupPlaceholder")}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="ec-maxc">{t("admin.creds.edit.maxConcurrent")}</Label>
              <Input
                id="ec-maxc"
                type="number"
                min={0}
                value={maxC}
                onChange={(e) => setMaxC(e.target.value)}
              />
            </div>
          </div>
          <div className="space-y-2">
            <Label htmlFor="ec-proxy">{t("admin.creds.edit.proxy")}</Label>
            <Input
              id="ec-proxy"
              value={proxy}
              onChange={(e) => setProxy(e.target.value)}
              placeholder={t("admin.creds.edit.proxyPlaceholder")}
              className="font-mono"
            />
          </div>
          {isAPIKey && (
            <>
              <div className="space-y-2">
                <Label htmlFor="ec-apikey">{t("admin.creds.edit.apiKey")}</Label>
                <Input
                  id="ec-apikey"
                  type="password"
                  value={apiKey}
                  onChange={(e) => setAPIKey(e.target.value)}
                  placeholder={t("admin.creds.edit.apiKeyPlaceholder")}
                  autoComplete="new-password"
                  className="font-mono"
                />
                <p className="text-[11px] text-muted-foreground">
                  {t("admin.creds.edit.apiKeyHint")}
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="ec-baseurl">{t("admin.creds.edit.baseUrl")}</Label>
                <Input
                  id="ec-baseurl"
                  value={base}
                  onChange={(e) => setBase(e.target.value)}
                  placeholder={t("admin.creds.edit.baseUrlPlaceholder")}
                  className="font-mono"
                />
              </div>
            </>
          )}
          {isAPIKey && <AllowedModelsField value={allowedModels} onChange={setAllowedModels} />}
          <div className="space-y-2">
            <Label htmlFor="ec-modelmap">{t("admin.creds.edit.modelMapJson")}</Label>
            <Textarea
              id="ec-modelmap"
              value={modelMap}
              onChange={(e) => setModelMap(e.target.value)}
              className="h-32 font-mono text-xs"
              placeholder={'{\n  "claude-opus-4-7": "claude-opus-4-8"\n}'}
            />
            <p className="text-[11px] text-muted-foreground">{t("admin.creds.modelMapHint")}</p>
            {/* The opus-4-6/4-7 → 4-8 default is Claude-only; showing it on a
                Codex credential describes a rewrite that never happens. */}
            {!isAPIKey && cred.provider === "anthropic" && (
              <p className="text-[11px] text-muted-foreground">
                {t("admin.creds.edit.oauthDefaultHint")}
              </p>
            )}
          </div>
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={disabled}
              onChange={(e) => setDisabled(e.target.checked)}
              className="size-4"
            />
            <span>{t("admin.creds.edit.disabledLabel")}</span>
          </label>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button disabled={busy} onClick={save}>
            {busy ? t("admin.creds.edit.saving") : t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
