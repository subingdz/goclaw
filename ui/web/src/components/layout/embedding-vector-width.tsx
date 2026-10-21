import { useState } from "react";
import { useTranslation } from "react-i18next";
import { AlertTriangle, CheckCircle2, Loader2, Ruler } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { useEmbeddingSchema, type RedimensionReport } from "@/hooks/use-embedding-schema";

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** The vector column width is a property of the database, so it is read from the server and
 * changed through the same code path the CLI uses. The flow is deliberately two-step: the
 * dry run reports how many rows will be destroyed, and the destructive step only becomes
 * possible after that number has been seen and the target typed back. */
export function EmbeddingVectorWidth() {
  const { t } = useTranslation("system-settings");
  const { schema, error, loading, refresh, plan, apply } = useEmbeddingSchema();

  const [target, setTarget] = useState("");
  const [report, setReport] = useState<RedimensionReport | null>(null);
  const [applied, setApplied] = useState<RedimensionReport | null>(null);
  const [force, setForce] = useState(false);
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState<"plan" | "apply" | "">("");
  const [failure, setFailure] = useState("");

  if (loading) return null;

  const maxIndexable = schema?.max_indexable_dimensions ?? 0;
  const to = Number(target);
  const targetValid = target.trim() !== ""
    && Number.isInteger(to) && to > 0
    && (maxIndexable === 0 || to <= maxIndexable);

  const runPlan = async () => {
    setBusy("plan"); setFailure(""); setApplied(null); setReport(null);
    try {
      setReport(await plan(to));
    } catch (err) {
      setFailure(messageOf(err));
    } finally {
      setBusy("");
    }
  };

  const runApply = async () => {
    setBusy("apply"); setFailure("");
    try {
      const result = await apply(to, force);
      setApplied(result);
      setReport(null);
      setConfirm("");
      setForce(false);
      await refresh();
    } catch (err) {
      // A refusal here (missing force, unreadable column, ceiling) keeps the plan on screen
      // so the operator can correct it instead of starting over.
      setFailure(messageOf(err));
    } finally {
      setBusy("");
    }
  };

  const needsConfirm = report?.needs_force ?? false;
  const canApply = !!report
    && report.columns.some((c) => c.action === "redimension")
    && confirm.trim() === String(report.target)
    && (!needsConfirm || force);

  return (
    <div className="space-y-3 rounded-lg border p-3">
      <div className="flex items-start gap-2">
        <Ruler className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" />
        <div className="flex-1 space-y-1">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <Label className="text-sm font-medium">{t("embedding.vectorWidth")}</Label>
            <span className="text-xs text-muted-foreground">
              {schema
                ? t("embedding.vectorWidthCurrent", { schema: schema.schema_dimensions })
                : t("embedding.vectorWidthUnknown")}
            </span>
          </div>

          {!schema ? (
            <p className="text-xs text-muted-foreground">
              {t("embedding.vectorWidthUnavailable", { reason: error || t("embedding.vectorWidthUnknown") })}
            </p>
          ) : (
            <>
              <ul className="space-y-0.5 text-xs text-muted-foreground">
                {schema.columns.map((c) => (
                  <li key={`${c.table}.${c.column}`} className="flex flex-wrap justify-between gap-2">
                    <span className="font-mono">{c.table}.{c.column}</span>
                    <span>{c.error || `vector(${c.dimensions}) · ${c.embedded} · ${c.indexes?.length ?? 0}`}</span>
                  </li>
                ))}
              </ul>
              <p className="text-xs text-muted-foreground">{t("embedding.vectorWidthHint")}</p>

              <div className="flex flex-wrap items-center gap-2">
                <Input
                  id="vectorWidthTarget"
                  type="number"
                  min={1}
                  value={target}
                  onChange={(e) => { setTarget(e.target.value); setReport(null); setFailure(""); }}
                  placeholder={t("embedding.vectorWidthTarget")}
                  className="w-40 text-base md:text-sm"
                />
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={!targetValid || busy !== ""}
                  onClick={runPlan}
                >
                  {busy === "plan" ? <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" /> : null}
                  {t("embedding.vectorWidthDryRun")}
                </Button>
              </div>

              {report ? (
                <div className="space-y-2 rounded-md border bg-muted/40 p-2.5 text-xs">
                  {report.unreadable > 0 ? (
                    <p className="flex items-center gap-1.5 text-amber-600 dark:text-amber-400">
                      <AlertTriangle className="h-3.5 w-3.5" />
                      {t("embedding.vectorWidthUnreadable", { count: report.unreadable })}
                    </p>
                  ) : null}

                  {report.columns.some((c) => c.action === "redimension") ? (
                    <>
                      <p className={report.embedded_rows > 0 ? "text-amber-600 dark:text-amber-400" : ""}>
                        {report.embedded_rows > 0
                          ? t("embedding.vectorWidthCost", { count: report.embedded_rows })
                          : t("embedding.vectorWidthNoVectors")}
                      </p>
                      <div className="space-y-1.5">
                        <p className="text-muted-foreground">
                          {report.columns
                            .filter((c) => c.action === "redimension")
                            .map((c) => `${c.table}.${c.column}: ${c.from} → ${c.to}`)
                            .join("  ·  ")}
                        </p>
                        {report.needs_force ? (
                          <label className="flex items-center gap-2">
                            <Switch checked={force} onCheckedChange={(v) => setForce(v)} />
                            <span>{t("embedding.vectorWidthForce")}</span>
                          </label>
                        ) : null}
                        <div className="flex flex-wrap items-center gap-2">
                          <Input
                            aria-label={t("embedding.vectorWidthConfirm", { to: report.target })}
                            value={confirm}
                            onChange={(e) => setConfirm(e.target.value)}
                            placeholder={t("embedding.vectorWidthConfirm", { to: report.target })}
                            className="w-40 text-base md:text-sm"
                          />
                          <Button
                            type="button"
                            size="sm"
                            variant="destructive"
                            disabled={!canApply || busy !== ""}
                            onClick={runApply}
                          >
                            {busy === "apply" ? <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" /> : null}
                            {t("embedding.vectorWidthApply")}
                          </Button>
                        </div>
                      </div>
                    </>
                  ) : (
                    <p className="text-muted-foreground">{t("embedding.vectorWidthAlready", { to: report.target })}</p>
                  )}
                </div>
              ) : null}

              {applied ? (
                <p className="flex items-start gap-1.5 text-xs text-emerald-600 dark:text-emerald-400">
                  <CheckCircle2 className="mt-0.5 h-3.5 w-3.5 shrink-0" />
                  {t("embedding.vectorWidthApplied", { to: applied.target, count: applied.columns.filter((c) => c.applied).length })}
                </p>
              ) : null}

              {failure ? (
                <p className="flex items-start gap-1.5 text-xs text-destructive">
                  <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
                  {failure}
                </p>
              ) : null}
            </>
          )}
        </div>
      </div>
    </div>
  );
}
