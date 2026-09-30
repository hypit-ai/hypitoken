import { Loader2, Send } from "lucide-react";
import { type FormEvent, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import { FadeIn } from "@/components/admin/fade-in";
import { GlassPanel } from "@/components/app/page-primitives";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { api } from "@/lib/api";
import { errMsg } from "@/lib/utils";

const MAX_PDF_SIZE = 8 * 1024 * 1024;

export function InvoicesTab() {
  const { t } = useTranslation();
  const [email, setEmail] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [sentTo, setSentTo] = useState("");
  const fileInput = useRef<HTMLInputElement>(null);
  const sending = useRef(false);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (sending.current || !file) return;
    if (!file.name.toLowerCase().endsWith(".pdf") || file.size === 0) {
      setError(t("admin.invoices.invalidPDF"));
      return;
    }
    if (file.size > MAX_PDF_SIZE) {
      setError(t("admin.invoices.tooLarge"));
      return;
    }
    sending.current = true;
    setBusy(true);
    setError("");
    setSentTo("");
    const recipient = email.trim();
    const body = new FormData();
    body.append("email", recipient);
    body.append("pdf", file);
    try {
      await api("/admin/invoices/send", { method: "POST", body });
      setSentTo(recipient);
      toast.success(t("admin.invoices.sent", { email: recipient }));
      setEmail("");
      setFile(null);
      if (fileInput.current) fileInput.current.value = "";
    } catch (e) {
      setError(errMsg(e));
    } finally {
      sending.current = false;
      setBusy(false);
    }
  };

  return (
    <FadeIn>
      <GlassPanel title={t("admin.invoices.title")} description={t("admin.invoices.description")}>
        <form onSubmit={submit} className="flex max-w-xl flex-col gap-6" aria-busy={busy}>
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="invoice-email">{t("admin.invoices.email")}</FieldLabel>
              <Input
                id="invoice-email"
                type="email"
                autoComplete="email"
                placeholder="customer@example.com"
                required
                maxLength={254}
                disabled={busy}
                value={email}
                onChange={(event) => setEmail(event.target.value)}
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="invoice-pdf">{t("admin.invoices.file")}</FieldLabel>
              <Input
                ref={fileInput}
                id="invoice-pdf"
                type="file"
                accept=".pdf,application/pdf"
                required
                disabled={busy}
                aria-describedby="invoice-file-hint"
                onChange={(event) => {
                  setFile(event.target.files?.[0] ?? null);
                  setError("");
                  setSentTo("");
                }}
              />
              <FieldDescription id="invoice-file-hint">
                {t("admin.invoices.fileHint")}
              </FieldDescription>
            </Field>
          </FieldGroup>
          {error ? (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          ) : null}
          {sentTo ? (
            <p role="status" className="text-sm text-success">
              {t("admin.invoices.sent", { email: sentTo })}
            </p>
          ) : null}
          <div className="flex flex-wrap items-center gap-4">
            <Button type="submit" disabled={busy || !email.trim() || !file}>
              {busy ? (
                <Loader2
                  data-icon="inline-start"
                  className="animate-spin motion-reduce:animate-none"
                />
              ) : (
                <Send data-icon="inline-start" />
              )}
              {t(busy ? "admin.invoices.sending" : "admin.invoices.send")}
            </Button>
            <Link
              to="/app/admin/tickets"
              className="text-sm text-primary underline-offset-4 hover:underline"
            >
              {t("admin.invoices.tickets")}
            </Link>
          </div>
        </form>
      </GlassPanel>
    </FadeIn>
  );
}
