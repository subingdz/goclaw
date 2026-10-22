import { useEffect } from "react";
import { useTranslation } from "react-i18next";
import { RefreshCw } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { useWsCall } from "@/hooks/use-ws-call";
import { formatRelativeTime } from "@/lib/format";
import type { ModelRoutingStatsResponse } from "@/types/agent";

interface ModelRoutingStatsSummaryProps {
  agentKey: string;
  /** Only worth asking while the card is on; an off card has nothing to attribute numbers to. */
  active: boolean;
}

/**
 * Live counters for this agent's routing policy (WS `agents.routingStats`).
 *
 * The states are deliberately separated because they read the same on a number line and mean
 * different things to an operator: a policy that is saved but has not run yet (the agent was just
 * edited, so the cached loop is gone), a policy that is live and matching nothing (thresholds too
 * high), and a policy that fires but changes nothing (it points at the model the agent already
 * uses). Only the last two are worth showing numbers for.
 */
export function ModelRoutingStatsSummary({ agentKey, active }: ModelRoutingStatsSummaryProps) {
  const { t } = useTranslation("agents");
  const s = "configSections.modelRouting.stats";
  const { data, loading, error, call } = useWsCall<ModelRoutingStatsResponse>("agents.routingStats");

  useEffect(() => {
    if (!active || !agentKey) return;
    // Swallowed on purpose: this block is a summary of a side effect, and a failed poll must not
    // look like a failed save. The error text below is the only signal it needs.
    call({ agentId: agentKey }).catch(() => {});
  }, [active, agentKey, call]);

  if (!active) return null;

  if (error) {
    return (
      <p className="text-xs text-muted-foreground">{t(`${s}.unavailable`)}</p>
    );
  }
  if (!data) {
    return (
      <p className="text-xs text-muted-foreground">
        {loading ? t(`${s}.loading`) : t(`${s}.unavailable`)}
      </p>
    );
  }
  if (!data.hasPolicy) {
    return <p className="text-xs text-muted-foreground">{t(`${s}.noPolicy`)}</p>;
  }

  const stats = data.cached ? data.stats : undefined;
  return (
    <div className="space-y-2 rounded-md border bg-muted/20 p-2.5">
      <div className="flex items-start justify-between gap-2">
        <p className="text-xs font-medium">{t(`${s}.title`)}</p>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="h-7 px-2 text-xs"
          disabled={loading}
          onClick={() => call({ agentId: agentKey }).catch(() => {})}
        >
          <RefreshCw className={`h-3.5 w-3.5 ${loading ? "animate-spin" : ""}`} />
          {t(`${s}.refresh`)}
        </Button>
      </div>

      {!stats ? (
        <p className="text-xs text-muted-foreground">{t(`${s}.savedNotLive`)}</p>
      ) : (
        <>
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
            <span className="text-muted-foreground">
              {t(`${s}.decisions`)} <span className="font-medium text-foreground">{stats.decisions}</span>
            </span>
            <span className="text-muted-foreground">
              {t(`${s}.changes`)} <span className="font-medium text-foreground">{stats.changes}</span>
            </span>
            <span className="text-muted-foreground">
              {t(`${s}.noMatch`)} <span className="font-medium text-foreground">{stats.noMatch}</span>
            </span>
            {stats.providerMisses > 0 && (
              <span className="text-warning">
                {t(`${s}.providerMisses`)} {stats.providerMisses}
              </span>
            )}
          </div>

          {Object.keys(stats.byTier ?? {}).length > 0 && (
            <div className="flex flex-wrap gap-1.5">
              {Object.entries(stats.byTier ?? {}).map(([tier, count]) => (
                <Badge key={tier} variant="secondary" className="h-5 text-[0.65rem] font-normal">
                  {tier} · {count}
                </Badge>
              ))}
            </div>
          )}

          {(stats.classifierCalls > 0 || stats.classifierFailures > 0) && (
            <p className="text-xs text-muted-foreground">
              {t(`${s}.classifier`)} {stats.classifierCalls}
              {" · "}
              {t(`${s}.classifierCacheHits`)} {stats.classifierCacheHits}
              {stats.classifierFailures > 0 && (
                <>
                  {" · "}
                  <span className="text-warning">
                    {t(`${s}.classifierFailures`)} {stats.classifierFailures}
                  </span>
                </>
              )}
            </p>
          )}

          {stats.windowStart && (
            <p className="text-xs text-muted-foreground">
              {t(`${s}.window`)} {formatRelativeTime(stats.windowStart)}
            </p>
          )}
        </>
      )}
    </div>
  );
}
