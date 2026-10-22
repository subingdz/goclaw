import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import {
  DndContext,
  PointerSensor,
  KeyboardSensor,
  closestCenter,
  useSensor,
  useSensors,
  type DragEndEvent,
} from "@dnd-kit/core";
import {
  SortableContext,
  arrayMove,
  sortableKeyboardCoordinates,
  verticalListSortingStrategy,
} from "@dnd-kit/sortable";
import { Plus } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Combobox } from "@/components/ui/combobox";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { InfoLabel } from "@/components/shared/info-label";
import type { ModelRoutingConfig, ModelRoutingTier } from "@/types/agent";
import { MODEL_ROUTING_MAX_TIERS } from "@/types/agent";
import type { ProviderData } from "@/types/provider";
import { SortableTierRow } from "./model-routing-tier-row";
import { ModelRoutingStatsSummary } from "./model-routing-stats";

interface ModelRoutingSectionProps {
  mode: "inherit" | "custom";
  onModeChange: (mode: "inherit" | "custom") => void;
  value: ModelRoutingConfig;
  onChange: (value: ModelRoutingConfig) => void;
  providers: ProviderData[];
  primaryProvider: string;
  primaryModel: string;
  primaryProviderId?: string;
  /** Live routing counters are fetched for this agent_key (WS agents.routingStats). */
  agentKey: string;
}

export function ModelRoutingSection({
  mode,
  onModeChange,
  value,
  onChange,
  providers,
  primaryProvider,
  primaryModel,
  primaryProviderId,
  agentKey,
}: ModelRoutingSectionProps) {
  const { t } = useTranslation("agents");
  const s = "configSections.modelRouting";
  const tiers = value.tiers ?? [];
  const disabled = mode === "inherit";

  const sensors = useSensors(
    useSensor(PointerSensor),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );
  const itemIds = tiers.map((_, index) => `routing-${index}`);
  const selectedProviderNames = useMemo(
    () => new Set(tiers.map((tier) => tier.provider).filter(Boolean)),
    [tiers],
  );
  const providerChoices = useMemo(
    () => providers.filter((provider) => provider.enabled || selectedProviderNames.has(provider.name)),
    [providers, selectedProviderNames],
  );

  const updateTier = (index: number, tier: ModelRoutingTier) => {
    const next = [...tiers];
    next[index] = tier;
    onChange({ ...value, tiers: next });
  };

  const removeTier = (index: number) => {
    onChange({ ...value, tiers: tiers.filter((_, tierIndex) => tierIndex !== index) });
  };

  const handleDragEnd = (event: DragEndEvent) => {
    const { active, over } = event;
    if (!over || active.id === over.id) return;
    const oldIndex = itemIds.indexOf(String(active.id));
    const newIndex = itemIds.indexOf(String(over.id));
    if (oldIndex < 0 || newIndex < 0) return;
    onChange({ ...value, tiers: arrayMove(tiers, oldIndex, newIndex) });
  };

  const classifier = value.classifier ?? null;

  return (
    <section className="space-y-3">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h3 className="text-sm font-medium">{t(`${s}.title`)}</h3>
          <p className="text-xs text-muted-foreground">{t(`${s}.description`)}</p>
        </div>
        <Select
          value={mode}
          onValueChange={(next) => {
            const chosen = next as "inherit" | "custom";
            onModeChange(chosen);
            // The stored policy has no separate "enabled" toggle — the card being on is what
            // enables it, so switching the card on must not save an inert enabled:false blob.
            if (chosen === "custom") onChange({ ...value, enabled: true });
          }}
        >
          <SelectTrigger className="w-32 text-base md:text-sm">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="inherit">{t(`${s}.off`)}</SelectItem>
            <SelectItem value="custom">{t(`${s}.on`)}</SelectItem>
          </SelectContent>
        </Select>
      </div>

      <div className="space-y-3 rounded-lg border p-3 sm:p-4">
        <div className="grid gap-2 rounded-md border bg-muted/30 p-2 sm:grid-cols-[auto_minmax(0,1fr)_minmax(0,1fr)]">
          <Badge variant="secondary" className="h-6 w-fit">
            {t(`${s}.baseline`)}
          </Badge>
          <div className="min-w-0 truncate text-sm">{primaryProvider}</div>
          <div className="min-w-0 truncate text-sm text-muted-foreground">{primaryModel}</div>
        </div>

        <ModelRoutingStatsSummary agentKey={agentKey} active={mode === "custom"} />

        {!disabled && tiers.length === 0 && (
          <p className="text-xs text-warning">{t(`${s}.noTiersWarning`)}</p>
        )}

        {tiers.length > 0 ? (
          <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={handleDragEnd}>
            <SortableContext items={itemIds} strategy={verticalListSortingStrategy}>
              <div className="space-y-2">
                {tiers.map((tier, index) => {
                  const id = itemIds[index] ?? `routing-${index}`;
                  return (
                    <SortableTierRow
                      key={id}
                      id={id}
                      index={index}
                      tier={tier}
                      providers={providerChoices}
                      ownProviderId={primaryProviderId}
                      onChange={(next) => updateTier(index, next)}
                      onRemove={() => removeTier(index)}
                    />
                  );
                })}
              </div>
            </SortableContext>
          </DndContext>
        ) : (
          <div className="rounded-md border border-dashed px-3 py-4 text-sm text-muted-foreground">
            {t(`${s}.empty`)}
          </div>
        )}

        <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <p className="text-xs text-muted-foreground">{t(`${s}.orderNote`)}</p>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={disabled || tiers.length >= MODEL_ROUTING_MAX_TIERS}
            onClick={() => onChange({ ...value, enabled: true, tiers: [...tiers, {}] })}
          >
            <Plus className="h-4 w-4" />
            {t(`${s}.add`)}
          </Button>
        </div>

        <div className="space-y-3 border-t pt-3">
          <div className="flex items-center justify-between gap-3">
            <div>
              <InfoLabel tip={t(`${s}.classifierTip`)}>{t(`${s}.classifier`)}</InfoLabel>
              <p className="text-xs text-muted-foreground">{t(`${s}.classifierDesc`)}</p>
            </div>
            <Switch
              id="agent-routing-classifier"
              disabled={disabled}
              checked={classifier !== null}
              onCheckedChange={(on) =>
                onChange({ ...value, classifier: on ? { model: "" } : null })
              }
            />
          </div>

          {classifier !== null && (
            <div className="space-y-3">
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div className="space-y-1.5">
                  <InfoLabel tip={t(`${s}.classifierProviderTip`)}>
                    {t(`${s}.classifierProvider`)}
                  </InfoLabel>
                  <Combobox
                    value={classifier.provider ?? ""}
                    onChange={(provider) =>
                      onChange({ ...value, classifier: { ...classifier, provider } })
                    }
                    options={[
                      { value: "", label: t(`${s}.ownProvider`) },
                      ...providerChoices.map((provider) => ({
                        value: provider.name,
                        label: provider.display_name || provider.name,
                      })),
                    ]}
                    placeholder={t(`${s}.ownProvider`)}
                  />
                </div>
                <div className="space-y-1.5">
                  <InfoLabel tip={t(`${s}.classifierModelTip`)}>
                    {t(`${s}.classifierModel`)}
                  </InfoLabel>
                  <Combobox
                    value={classifier.model ?? ""}
                    onChange={(model) => onChange({ ...value, classifier: { ...classifier, model } })}
                    options={[]}
                    placeholder={t(`${s}.classifierModelPlaceholder`)}
                    allowCustom
                  />
                </div>
              </div>
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div className="space-y-1.5">
                  <InfoLabel tip={t(`${s}.timeoutTip`)}>{t(`${s}.timeout`)}</InfoLabel>
                  <Input
                    className="text-base md:text-sm"
                    type="number"
                    min={0}
                    disabled={disabled}
                    value={classifier.timeout_ms ?? 0}
                    onChange={(event) =>
                      onChange({
                        ...value,
                        classifier: {
                          ...classifier,
                          timeout_ms: Math.max(0, Math.trunc(Number(event.target.value))),
                        },
                      })
                    }
                  />
                </div>
                <div className="space-y-1.5">
                  <InfoLabel tip={t(`${s}.cacheTtlTip`)}>{t(`${s}.cacheTtl`)}</InfoLabel>
                  <Input
                    className="text-base md:text-sm"
                    type="number"
                    min={0}
                    disabled={disabled}
                    value={classifier.cache_ttl_seconds ?? 0}
                    onChange={(event) =>
                      onChange({
                        ...value,
                        classifier: {
                          ...classifier,
                          cache_ttl_seconds: Math.max(0, Math.trunc(Number(event.target.value))),
                        },
                      })
                    }
                  />
                </div>
              </div>
              <div className="grid grid-cols-1 gap-1.5 sm:grid-cols-2">
                <p className="text-xs text-muted-foreground">{t(`${s}.timeoutDefaultNote`)}</p>
                <p className="text-xs text-muted-foreground">{t(`${s}.cacheTtlDefaultNote`)}</p>
              </div>
              {!(classifier.model ?? "").trim() && (
                <p className="text-xs text-warning">{t(`${s}.classifierNeedsModel`)}</p>
              )}
            </div>
          )}
        </div>
      </div>
    </section>
  );
}
