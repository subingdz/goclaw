import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Search, ArrowLeft, ShieldAlert, ExternalLink } from "lucide-react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { toast } from "@/stores/use-toast-store";
import i18next from "i18next";
import { userFriendlyError } from "@/lib/error-utils";
import { useSkillRegistry, type RegistrySummary, type RegistryAnalysis } from "./hooks/use-skill-registry";

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Called after a successful install so the skill list refreshes and the new row appears under Custom. */
  onInstalled: () => void;
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

export function SkillRegistryDialog({ open, onOpenChange, onInstalled }: Props) {
  const { t } = useTranslation("skills");
  const reg = useSkillRegistry(open);
  const [query, setQuery] = useState("");
  const [sourceFilter, setSourceFilter] = useState("");
  const [results, setResults] = useState<RegistrySummary[]>([]);
  const [failures, setFailures] = useState<Record<string, string>>({});
  const [searched, setSearched] = useState(false);
  const [detail, setDetail] = useState<{ item: RegistrySummary; analysis: RegistryAnalysis; skillMd: string } | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);

  const close = (next: boolean) => {
    if (reg.installing) return;
    if (!next) {
      setDetail(null);
      setResults([]);
      setFailures({});
      setSearched(false);
      setQuery("");
    }
    onOpenChange(next);
  };

  const runSearch = async () => {
    const q = query.trim();
    if (!q) return;
    setDetail(null);
    try {
      const res = await reg.search(q, sourceFilter || undefined);
      setResults(res.results ?? []);
      setFailures(res.failures ?? {});
      setSearched(true);
    } catch (err) {
      toast.error(i18next.t("skills:registry.searchFailed"), userFriendlyError(err));
    }
  };

  const openDetail = async (item: RegistrySummary) => {
    setDetailLoading(true);
    try {
      const res = await reg.inspect({ source: item.source, slug: item.slug, version: item.version });
      setDetail({ item, analysis: res.analysis, skillMd: res.skill_md ?? "" });
    } catch (err) {
      toast.error(i18next.t("skills:registry.inspectFailed"), userFriendlyError(err));
    } finally {
      setDetailLoading(false);
    }
  };

  const runInstall = async () => {
    if (!detail) return;
    try {
      const res = await reg.install({ source: detail.item.source, slug: detail.item.slug, version: detail.item.version });
      if (res.unchanged) {
        toast.info(i18next.t("skills:registry.alreadyInstalled", { slug: res.slug, version: res.version }));
      } else {
        toast.success(i18next.t("skills:registry.installed", { slug: res.slug, version: res.version }));
        if (res.status === "archived") {
          toast.warning(i18next.t("skills:registry.installedArchived", { deps: (res.missing_deps ?? []).join(", ") }));
        }
      }
      onInstalled();
      setDetail(null);
    } catch (err) {
      toast.error(i18next.t("skills:registry.installFailed"), userFriendlyError(err));
    }
  };

  const searchable = reg.sources.filter((s) => s.searchable);

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="max-h-[85dvh] flex flex-col">
        <DialogHeader>
          <DialogTitle>{detail ? t("registry.detailTitle") : t("registry.title")}</DialogTitle>
          <DialogDescription>
            {reg.enabled
              ? detail
                ? t("registry.detailHint")
                : t("registry.description")
              : t("registry.disabled")}
          </DialogDescription>
        </DialogHeader>

        {!reg.enabled && !reg.sourcesLoading ? (
          <div className="space-y-2 text-sm text-muted-foreground">
            <p>{t("registry.enableHint")}</p>
            <code className="block rounded-md bg-muted p-2 text-xs">
              skills.registries.clawhub.enabled = true
            </code>
          </div>
        ) : detail ? (
          <div className="min-h-0 flex-1 space-y-3 overflow-y-auto">
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0">
                <p className="truncate text-sm font-medium">{detail.analysis.name}</p>
                <p className="text-xs text-muted-foreground">
                  {detail.analysis.source}:{detail.analysis.locator}
                  {detail.analysis.version ? ` @ ${detail.analysis.version}` : ""}
                </p>
              </div>
              <Button variant="ghost" size="sm" className="gap-1" onClick={() => setDetail(null)}>
                <ArrowLeft className="h-3.5 w-3.5" /> {t("registry.back")}
              </Button>
            </div>

            {detail.analysis.description && (
              <p className="text-sm">{detail.analysis.description}</p>
            )}

            <div className="flex flex-wrap gap-1.5">
              <Badge variant={detail.analysis.unsafe ? "destructive" : "success"}>
                {detail.analysis.unsafe ? t("registry.badgeUnsafe") : t("registry.badgeClean")}
              </Badge>
              {detail.analysis.suspicious && <Badge variant="warning">{t("registry.badgeSuspicious")}</Badge>}
              {detail.analysis.needs_shell && <Badge variant="outline">{t("registry.badgeShell")}</Badge>}
              {(detail.analysis.executables?.length ?? 0) > 0 && (
                <Badge variant="outline">{t("registry.badgeExec", { count: detail.analysis.executables!.length })}</Badge>
              )}
              {(detail.analysis.missing_deps?.length ?? 0) > 0 && (
                <Badge variant="warning">{t("registry.badgeMissingDeps", { deps: detail.analysis.missing_deps!.join(", ") })}</Badge>
              )}
            </div>

            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div className="rounded-md border p-3">
                <p className="mb-1 text-xs font-medium text-muted-foreground">
                  {t("registry.files", { count: detail.analysis.files.length, size: formatBytes(detail.analysis.total_bytes) })}
                </p>
                <ul className="max-h-40 space-y-0.5 overflow-y-auto text-xs">
                  {detail.analysis.files.map((f) => (
                    <li key={f.path} className="flex justify-between gap-2">
                      <span className="min-w-0 truncate font-mono">{f.path}</span>
                      <span className="shrink-0 text-muted-foreground">{formatBytes(f.bytes)}</span>
                    </li>
                  ))}
                </ul>
              </div>
              <div className="rounded-md border p-3">
                <p className="mb-1 text-xs font-medium text-muted-foreground">{t("registry.declaredDeps")}</p>
                {(detail.analysis.declared?.length ?? 0) === 0 ? (
                  <p className="text-xs text-muted-foreground">{t("registry.noDeps")}</p>
                ) : (
                  <ul className="text-xs">
                    {detail.analysis.declared!.map((d) => (
                      <li key={d} className="font-mono">{d}</li>
                    ))}
                  </ul>
                )}
                {detail.analysis.url && (
                  <a
                    href={detail.analysis.url}
                    target="_blank"
                    rel="noreferrer"
                    className="mt-2 inline-flex items-center gap-1 text-xs text-primary"
                  >
                    <ExternalLink className="h-3 w-3" /> {t("registry.upstreamPage")}
                  </a>
                )}
              </div>
            </div>

            {(detail.analysis.findings?.length ?? 0) > 0 && (
              <div className="rounded-md border border-destructive/40 bg-destructive/5 p-3">
                <p className="mb-1 flex items-center gap-1.5 text-xs font-medium text-destructive">
                  <ShieldAlert className="h-3.5 w-3.5" /> {t("registry.findings")}
                </p>
                <ul className="space-y-0.5 text-xs">
                  {detail.analysis.findings!.map((f) => (
                    <li key={f} className="font-mono">{f}</li>
                  ))}
                </ul>
              </div>
            )}

            <div className="rounded-md border">
              <p className="border-b px-3 py-1.5 text-xs font-medium text-muted-foreground">SKILL.md</p>
              <pre className="max-h-56 overflow-auto whitespace-pre-wrap p-3 text-xs">{detail.skillMd}</pre>
            </div>
          </div>
        ) : (
          <div className="min-h-0 flex-1 space-y-3">
            <div className="flex gap-2">
              <Input
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") void runSearch();
                }}
                placeholder={t("registry.searchPlaceholder")}
                className="text-base md:text-sm"
              />
              <Button size="sm" onClick={runSearch} disabled={reg.searching || !query.trim()} className="gap-1">
                <Search className="h-3.5 w-3.5" /> {reg.searching ? t("registry.searching") : t("registry.search")}
              </Button>
            </div>

            {searchable.length > 1 && (
              <div className="flex flex-wrap gap-1">
                {[{ name: "", label: t("registry.allSources") }, ...searchable.map((s) => ({ name: s.name, label: s.name }))].map((chip) => (
                  <button
                    key={chip.name || "all"}
                    type="button"
                    onClick={() => setSourceFilter(chip.name)}
                    className={cn(
                      "rounded-full border px-3 py-1.5 text-xs",
                      sourceFilter === chip.name ? "border-primary text-primary" : "text-muted-foreground",
                    )}
                  >
                    {chip.label}
                  </button>
                ))}
              </div>
            )}

            {Object.keys(failures).length > 0 && (
              <div className="rounded-md border border-warning/40 bg-warning/5 p-2 text-xs">
                {Object.entries(failures).map(([src, reason]) => (
                  <p key={src}>
                    <span className="font-medium">{src}</span>: {reason}
                  </p>
                ))}
              </div>
            )}

            <div className="min-h-0 flex-1 overflow-y-auto">
              {searched && results.length === 0 && (
                <p className="py-6 text-center text-sm text-muted-foreground">{t("registry.noResults")}</p>
              )}
              <ul className="divide-y">
                {results.map((item) => (
                  <li key={`${item.source}/${item.slug}`} className="flex items-start justify-between gap-3 py-2.5">
                    <div className="min-w-0">
                      <div className="flex flex-wrap items-center gap-1.5">
                        <span className="truncate text-sm font-medium">{item.display_name || item.slug}</span>
                        <Badge variant="outline">{item.source}</Badge>
                        {item.suspicious && <Badge variant="warning">{t("registry.badgeSuspicious")}</Badge>}
                        {item.malware_blocked && <Badge variant="destructive">{t("registry.badgeMalware")}</Badge>}
                      </div>
                      <p className="mt-0.5 line-clamp-2 text-xs text-muted-foreground">{item.summary}</p>
                    </div>
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => openDetail(item)}
                      disabled={detailLoading || item.malware_blocked}
                    >
                      {t("registry.inspect")}
                    </Button>
                  </li>
                ))}
              </ul>
            </div>
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => close(false)} disabled={reg.installing}>
            {t("registry.close")}
          </Button>
          {detail && (
            <Button onClick={runInstall} disabled={detail.analysis.unsafe || reg.installing}>
              {reg.installing ? t("registry.installing") : t("registry.install")}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
