import { useTranslation } from "react-i18next";
import { useSortable } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { GripVertical, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Combobox } from "@/components/ui/combobox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { InfoLabel } from "@/components/shared/info-label";
import { useProviderModels } from "@/pages/providers/hooks/use-provider-models";
import type { ProviderData } from "@/types/provider";
import type { ModelRoutingTier } from "@/types/agent";

const RUN_KINDS = ["user", "delegation", "announce"] as const;

interface SortableTierRowProps {
  id: string;
  index: number;
  tier: ModelRoutingTier;
  providers: ProviderData[];
  /** Used for the model list when the tier names no provider, i.e. "the agent's own". */
  ownProviderId?: string;
  onChange: (tier: ModelRoutingTier) => void;
  onRemove: () => void;
}

export function SortableTierRow({
  id,
  index,
  tier,
  providers,
  ownProviderId,
  onChange,
  onRemove,
}: SortableTierRowProps) {
  const { t } = useTranslation("agents");
  const s = "configSections.modelRouting";
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id });

  const selectedProvider = providers.find((provider) => provider.name === tier.provider);
  const { models } = useProviderModels(selectedProvider?.id || ownProviderId);
  const modelOptions = models.map((model) => ({ value: model.id, label: model.name || model.id }));

  const patch = (updates: Partial<ModelRoutingTier>) => onChange({ ...tier, ...updates });
  const toggleRunKind = (kind: string) => {
    const current = tier.run_kinds ?? [];
    patch({
      run_kinds: current.includes(kind)
        ? current.filter((entry) => entry !== kind)
        : [...current, kind],
    });
  };

  return (
    <div
      ref={setNodeRef}
      style={{ transform: CSS.Transform.toString(transform), transition }}
      className={`space-y-3 rounded-md border bg-background p-3 ${isDragging ? "shadow-md" : ""}`}
    >
      <div className="flex items-center gap-2">
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="h-9 w-9 shrink-0 cursor-grab text-muted-foreground active:cursor-grabbing"
          aria-label={t(`${s}.reorder`)}
          {...attributes}
          {...listeners}
        >
          <GripVertical className="h-4 w-4" />
        </Button>
        <span className="w-5 shrink-0 text-xs text-muted-foreground">{index + 1}</span>
        <Input
          className="text-base md:text-sm"
          value={tier.name ?? ""}
          onChange={(event) => patch({ name: event.target.value })}
          placeholder={t(`${s}.namePlaceholder`)}
          aria-label={t(`${s}.name`)}
        />
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="ml-auto h-9 w-9 shrink-0 text-muted-foreground hover:text-destructive"
          aria-label={t(`${s}.remove`)}
          onClick={onRemove}
        >
          <Trash2 className="h-4 w-4" />
        </Button>
      </div>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div className="space-y-1.5">
          <InfoLabel tip={t(`${s}.providerTip`)}>{t(`${s}.provider`)}</InfoLabel>
          <Combobox
            value={tier.provider ?? ""}
            onChange={(provider) => patch({ provider, model: "" })}
            options={[
              { value: "", label: t(`${s}.ownProvider`) },
              ...providers.map((provider) => ({
                value: provider.name,
                label: provider.display_name || provider.name,
              })),
            ]}
            placeholder={t(`${s}.ownProvider`)}
          />
        </div>
        <div className="space-y-1.5">
          <InfoLabel tip={t(`${s}.modelTip`)}>{t(`${s}.model`)}</InfoLabel>
          <Combobox
            value={tier.model ?? ""}
            onChange={(model) => patch({ model })}
            options={modelOptions}
            placeholder={t(`${s}.modelPlaceholder`)}
            allowCustom
          />
        </div>
      </div>

      <div className="space-y-1.5">
        <InfoLabel tip={t(`${s}.hintTip`)}>{t(`${s}.hint`)}</InfoLabel>
        <Input
          className="text-base md:text-sm"
          value={tier.hint ?? ""}
          onChange={(event) => patch({ hint: event.target.value })}
          placeholder={t(`${s}.hintPlaceholder`)}
        />
      </div>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div className="space-y-1.5">
          <InfoLabel tip={t(`${s}.minRunesTip`)}>{t(`${s}.minRunes`)}</InfoLabel>
          <Input
            className="text-base md:text-sm"
            type="number"
            min={0}
            value={tier.min_runes ?? 0}
            onChange={(event) => patch({ min_runes: Math.max(0, Math.trunc(Number(event.target.value))) })}
          />
        </div>
        <div className="space-y-1.5">
          <InfoLabel tip={t(`${s}.minTokensTip`)}>{t(`${s}.minTokens`)}</InfoLabel>
          <Input
            className="text-base md:text-sm"
            type="number"
            min={0}
            value={tier.min_tokens ?? 0}
            onChange={(event) => patch({ min_tokens: Math.max(0, Math.trunc(Number(event.target.value))) })}
          />
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-x-5 gap-y-3">
        <div className="flex items-center gap-2">
          <Switch
            id={`${id}-code`}
            checked={Boolean(tier.requires_code)}
            onCheckedChange={(requiresCode) => patch({ requires_code: requiresCode })}
          />
          <Label htmlFor={`${id}-code`} className="text-xs font-normal text-muted-foreground">
            {t(`${s}.requiresCode`)}
          </Label>
        </div>
        <div className="flex items-center gap-2">
          <Switch
            id={`${id}-media`}
            checked={Boolean(tier.requires_media)}
            onCheckedChange={(requiresMedia) => patch({ requires_media: requiresMedia })}
          />
          <Label htmlFor={`${id}-media`} className="text-xs font-normal text-muted-foreground">
            {t(`${s}.requiresMedia`)}
          </Label>
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="text-xs text-muted-foreground">{t(`${s}.runKinds`)}</span>
          {RUN_KINDS.map((kind) => {
            const active = (tier.run_kinds ?? []).includes(kind);
            return (
              <Button
                key={kind}
                type="button"
                size="sm"
                variant={active ? "secondary" : "ghost"}
                className="h-7 px-2 text-xs"
                aria-pressed={active}
                onClick={() => toggleRunKind(kind)}
              >
                {t(`${s}.runKind.${kind}`, { defaultValue: kind })}
              </Button>
            );
          })}
        </div>
      </div>
    </div>
  );
}
