import { useTranslation } from "react-i18next";
import { Loader2, CheckCircle2, XCircle, AlertTriangle } from "lucide-react";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Button } from "@/components/ui/button";
import { useEmbeddingStatus } from "@/hooks/use-embedding-status";

import type { VerifyResult } from "../hooks/use-provider-verify";

interface ProviderEmbeddingSectionProps {
  embEnabled: boolean;
  setEmbEnabled: (v: boolean) => void;
  embModel: string;
  setEmbModel: (v: string) => void;
  embApiBase: string;
  setEmbApiBase: (v: string) => void;
  embDims: string;
  setEmbDims: (v: string) => void;
  onVerify: () => void;
  verifying: boolean;
  verifyResult: VerifyResult | null;
}

export function ProviderEmbeddingSection({
  embEnabled,
  setEmbEnabled,
  embModel,
  setEmbModel,
  embApiBase,
  setEmbApiBase,
  embDims,
  setEmbDims,
  onVerify,
  verifying,
  verifyResult,
}: ProviderEmbeddingSectionProps) {
  const { t } = useTranslation("providers");
  // The stored width belongs to this deployment's database, not to the app build, so it
  // is asked for rather than hard-coded into the page.
  const { status } = useEmbeddingStatus();
  const schemaDims = status?.schema_dimensions ?? 0;
  const maxIndexable = status?.max_indexable_dimensions ?? 0;

  // Client-side mirror of what the server will refuse, so the reason shows up before the
  // save round-trip. This field cannot change the stored width — it only has to agree with
  // it, which is what the sentence below says.
  const parsedDims = embDims.trim() === "" ? null : Number(embDims);
  const dimsError = !embEnabled || parsedDims === null
    ? ""
    : !Number.isFinite(parsedDims) || parsedDims <= 0
      ? t("embedding.dimensionsInvalidNumber")
      : maxIndexable > 0 && parsedDims > maxIndexable
        ? t("embedding.dimensionsCeiling", { max: maxIndexable })
        : schemaDims > 0 && parsedDims !== schemaDims
          ? t("embedding.dimensionsNotStoredWidth", { schema: schemaDims })
          : "";

  return (
    <section className="space-y-3 rounded-lg border p-3 sm:p-4 overflow-hidden">
      <h3 className="text-sm font-medium">{t("detail.embeddingSection")}</h3>
      <div className="flex items-center justify-between gap-4">
        <div className="space-y-0.5">
          <Label htmlFor="embEnabled" className="text-sm font-medium">
            {t("embedding.enable")}
          </Label>
          <p className="text-xs text-muted-foreground">{t("embedding.enableDesc")}</p>
        </div>
        <Switch id="embEnabled" checked={embEnabled} onCheckedChange={setEmbEnabled} />
      </div>

      {embEnabled ? (
        <div className="space-y-3 pt-1">
          <div className="space-y-2">
            <Label htmlFor="embModel">{t("embedding.model")}</Label>
            <Input
              id="embModel"
              value={embModel}
              onChange={(e) => setEmbModel(e.target.value)}
              placeholder="text-embedding-3-small"
              className="text-base md:text-sm"
            />
          </div>

          <div className="space-y-2">
            <div className="flex items-baseline justify-between gap-3">
              <Label htmlFor="embDims">{t("embedding.dimensions")}</Label>
              <span className="text-xs text-muted-foreground">
                {schemaDims > 0
                  ? t("embedding.storedWidth", { schema: schemaDims })
                  : t("embedding.storedWidthNone")}
              </span>
            </div>
            <Input
              id="embDims"
              type="number"
              min={1}
              value={embDims}
              onChange={(e) => setEmbDims(e.target.value)}
              placeholder={t("embedding.dimensionsPlaceholder")}
              className="text-base md:text-sm"
            />
            <p className={`text-xs ${dimsError ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground"}`}>
              {dimsError || (schemaDims > 0
                ? t("embedding.dimensionsHint", { schema: schemaDims })
                : t("embedding.dimensionsHintNoSchema"))}
            </p>
            {verifyResult?.remediation ? (
              <p className="text-xs text-muted-foreground">
                {t("embedding.remediation")}{" "}
                <code className="break-all rounded bg-muted px-1 py-0.5 text-[11px]">{verifyResult.remediation}</code>
              </p>
            ) : null}
          </div>

          <div className="space-y-2">
            <Label htmlFor="embApiBase">{t("embedding.apiBase")}</Label>
            <Input
              id="embApiBase"
              value={embApiBase}
              onChange={(e) => setEmbApiBase(e.target.value)}
              placeholder={t("embedding.apiBasePlaceholder")}
              className="text-base md:text-sm"
            />
            <p className="text-xs text-muted-foreground">{t("embedding.apiBaseHint")}</p>
          </div>

          <div className="flex items-center gap-3">
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={verifying}
              onClick={onVerify}
            >
              {verifying ? <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" /> : null}
              {t("embedding.verify")}
            </Button>
            {verifyResult ? (
              <span
                className={`flex items-center gap-1 text-xs ${
                  verifyResult.valid
                    ? verifyResult.dimension_mismatch
                      ? "text-amber-600 dark:text-amber-400"
                      : "text-success"
                    : "text-destructive"
                }`}
              >
                {verifyResult.valid ? (
                  <>
                    {verifyResult.dimension_mismatch ? (
                      <AlertTriangle className="h-3.5 w-3.5" />
                    ) : (
                      <CheckCircle2 className="h-3.5 w-3.5" />
                    )}
                    {verifyResult.dimension_mismatch
                      ? t("embedding.dimensionsMismatch", {
                          count: verifyResult.dimensions,
                          schema: verifyResult.schema_dimensions ?? schemaDims,
                        })
                      : `${verifyResult.dimensions} dimensions`}
                  </>
                ) : (
                  <>
                    <XCircle className="h-3.5 w-3.5" />
                    {verifyResult.error || t("embedding.verifyFailed")}
                  </>
                )}
              </span>
            ) : null}
          </div>
        </div>
      ) : null}
    </section>
  );
}
