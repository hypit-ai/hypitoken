import { useState } from "react";
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
import { apiPost } from "@/lib/api";
import { errMsg } from "@/lib/utils";
import { AllowedModelsField, parseAllowedModels } from "./allowed-models-field";

export interface CredentialDialogProps {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  onCreated: () => void;
}

export function AddAPIKeyDialog({
  open,
  onOpenChange,
  provider,
  onCreated,
}: CredentialDialogProps & { provider: string }) {
  const { t } = useTranslation();
  const [key, setKey] = useState("");
  const [label, setLabel] = useState("");
  const [base, setBase] = useState("");
  const [proxy, setProxy] = useState("");
  const [group, setGroup] = useState("");
  const [modelMap, setModelMap] = useState("");
  const [allowedModels, setAllowedModels] = useState("");
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-[500px]">
        <DialogHeader>
          <DialogTitle>
            {t("admin.creds.newApiTitle")} ·{" "}
            {provider === "openai" ? "Codex (OpenAI)" : "Claude (Anthropic)"}
          </DialogTitle>
          <DialogDescription>{t("admin.creds.newApiSub")}</DialogDescription>
        </DialogHeader>
        <div className="grid gap-3 py-2">
          <div className="space-y-2">
            <Label htmlFor="ak-1">{t("admin.creds.edit.apiKey")}</Label>
            <Input
              id="ak-1"
              type="password"
              value={key}
              onChange={(e) => setKey(e.target.value)}
              placeholder={t("admin.creds.newApiPlaceholder")}
              className="font-mono"
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="ak-2">{t("admin.creds.cols.label")}</Label>
            <Input
              id="ak-2"
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              placeholder={t("admin.creds.newApiLabelPlaceholder")}
            />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-2">
              <Label htmlFor="ak-3">{t("admin.creds.edit.baseUrl")}</Label>
              <Input
                id="ak-3"
                value={base}
                onChange={(e) => setBase(e.target.value)}
                placeholder={t("admin.creds.newApiBaseUrlPlaceholder")}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="ak-4">{t("admin.creds.edit.proxy")}</Label>
              <Input id="ak-4" value={proxy} onChange={(e) => setProxy(e.target.value)} />
            </div>
          </div>
          <div className="space-y-2">
            <Label htmlFor="ak-5">{t("admin.creds.cols.group")}</Label>
            <Input id="ak-5" value={group} onChange={(e) => setGroup(e.target.value)} />
          </div>
          <AllowedModelsField value={allowedModels} onChange={setAllowedModels} />
          <div className="space-y-2">
            <Label htmlFor="ak-6">{t("admin.creds.modelMapLabel")}</Label>
            <Textarea
              id="ak-6"
              value={modelMap}
              onChange={(e) => setModelMap(e.target.value)}
              placeholder={'{\n  "claude-opus-4-6": "claude-opus-4-8"\n}'}
              className="min-h-[72px] font-mono text-xs"
            />
            <p className="text-[11px] text-muted-foreground">{t("admin.creds.modelMapHint")}</p>
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button
            onClick={async () => {
              if (!key) {
                toast.error(t("common.error"));
                return;
              }
              let model_map: Record<string, string> = {};
              if (modelMap.trim() !== "") {
                try {
                  model_map = JSON.parse(modelMap);
                } catch (e) {
                  toast.error(t("admin.creds.modelMapParseError", { msg: errMsg(e) }));
                  return;
                }
              }
              try {
                await apiPost("/admin/credentials/apikey", {
                  provider,
                  api_key: key,
                  label,
                  base_url: base,
                  proxy_url: proxy,
                  group,
                  model_map,
                  allowed_models: parseAllowedModels(allowedModels),
                });
                toast.success(t("admin.creds.newApiCreated"));
                onCreated();
                onOpenChange(false);
                setKey("");
                setLabel("");
                setBase("");
                setProxy("");
                setGroup("");
                setModelMap("");
                setAllowedModels("");
              } catch (e) {
                toast.error(errMsg(e));
              }
            }}
          >
            {t("common.add")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
