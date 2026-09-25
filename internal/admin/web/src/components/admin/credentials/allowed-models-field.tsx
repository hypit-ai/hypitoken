import { useId } from "react";
import { useTranslation } from "react-i18next";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Textarea } from "@/components/ui/textarea";

export function parseAllowedModels(text: string): string[] {
  return [
    ...new Set(
      text
        .split(/[\n,，]+/)
        .map((model) => model.trim())
        .filter(Boolean),
    ),
  ];
}

export function AllowedModelsField({
  value,
  onChange,
}: {
  value: string;
  onChange: (value: string) => void;
}) {
  const { t } = useTranslation();
  const id = useId();
  return (
    <FieldGroup>
      <Field>
        <FieldLabel htmlFor={id}>{t("admin.creds.allowedModelsLabel")}</FieldLabel>
        <Textarea
          id={id}
          value={value}
          onChange={(event) => onChange(event.target.value)}
          rows={3}
          spellCheck={false}
          placeholder={"gpt-6-astra\ngpt-6-sol"}
          aria-describedby={`${id}-hint`}
        />
        <FieldDescription id={`${id}-hint`}>{t("admin.creds.allowedModelsHint")}</FieldDescription>
      </Field>
    </FieldGroup>
  );
}
