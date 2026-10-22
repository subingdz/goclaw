// Speech-to-text (audio.stt) configuration section.
// The API key is a secret: it round-trips through the config_secrets table and arrives
// masked, so a masked value must never be sent back — that would store the mask itself.

import { useEffect, useState } from "react";
import { Save } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";

type SttData = Record<string, any>;

interface Props {
  data: SttData | undefined;
  onSave: (value: SttData) => Promise<void>;
  saving: boolean;
}

const PROVIDERS = ["openai_compat", "elevenlabs", "proxy"];

function isMasked(val: unknown): boolean {
  return typeof val === "string" && val.includes("***");
}

export function SttSection({ data, onSave, saving }: Props) {
  const { t } = useTranslation("config");
  const [draft, setDraft] = useState<SttData>(data?.stt ?? {});
  const [dirty, setDirty] = useState(false);

  useEffect(() => {
    setDraft(data?.stt ?? {});
    setDirty(false);
  }, [data]);

  const update = (patch: SttData) => {
    setDraft((prev) => ({ ...prev, ...patch }));
    setDirty(true);
  };

  const provider = typeof draft.provider === "string" ? draft.provider : "";
  const keyMasked = isMasked(draft.api_key);

  const handleSave = () => {
    const toSave: SttData = { ...draft };
    // A masked secret is not a value. Sending it back would store the mask.
    if (keyMasked) delete toSave.api_key;
    for (const key of Object.keys(toSave)) {
      if (toSave[key] === undefined || toSave[key] === "") delete toSave[key];
    }
    onSave({ stt: toSave }).catch(() => {});
  };

  return (
    <Card>
      <CardHeader className="pb-3">
        <div className="flex items-center gap-2">
          <CardTitle className="text-base">{t("stt.title")}</CardTitle>
          <Badge variant={provider ? "default" : "secondary"}>
            {provider ? t("stt.configured", { provider }) : t("stt.notConfigured")}
          </Badge>
        </div>
        <CardDescription>{t("stt.description")}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div className="grid gap-1.5">
            <Label htmlFor="sttProvider">{t("stt.provider")}</Label>
            <Select value={provider || "none"} onValueChange={(v) => update({ provider: v === "none" ? "" : v })}>
              <SelectTrigger id="sttProvider">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="none">{t("stt.providerNone")}</SelectItem>
                {PROVIDERS.map((p) => (
                  <SelectItem key={p} value={p}>{t(`stt.providerKinds.${p}`)}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">{t("stt.providerHint")}</p>
          </div>

          <div className="grid gap-1.5">
            <Label htmlFor="sttFallback">{t("stt.fallback")}</Label>
            <Select value={typeof draft.fallback === "string" && draft.fallback ? draft.fallback : "none"}
              onValueChange={(v) => update({ fallback: v === "none" ? "" : v })}>
              <SelectTrigger id="sttFallback">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="none">{t("stt.fallbackNone")}</SelectItem>
                {PROVIDERS.map((p) => (
                  <SelectItem key={p} value={p}>{t(`stt.providerKinds.${p}`)}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">{t("stt.fallbackHint")}</p>
          </div>
        </div>

        <div className="grid gap-1.5">
          <Label htmlFor="sttBaseUrl">{t("stt.baseUrl")}</Label>
          <Input
            id="sttBaseUrl"
            value={draft.base_url ?? ""}
            placeholder="https://api.siliconflow.cn/v1"
            onChange={(e) => update({ base_url: e.target.value })}
            className="text-base md:text-sm"
          />
          <p className="text-xs text-muted-foreground">{t("stt.baseUrlHint")}</p>
        </div>

        <div className="grid gap-1.5">
          <Label htmlFor="sttSilk">{t("stt.silkDecoder")}</Label>
          <Input
            id="sttSilk"
            value={draft.silk_decoder ?? ""}
            placeholder="/usr/local/bin/silk_v3_decoder"
            onChange={(e) => update({ silk_decoder: e.target.value })}
            className="text-base md:text-sm"
          />
          <p className="text-xs text-muted-foreground">{t("stt.silkDecoderHint")}</p>
        </div>

        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div className="grid gap-1.5">
            <Label htmlFor="sttModel">{t("stt.model")}</Label>
            <Input
              id="sttModel"
              value={draft.model ?? ""}
              placeholder="FunAudioLLM/SenseVoiceSmall"
              onChange={(e) => update({ model: e.target.value })}
              className="text-base md:text-sm"
            />
            <p className="text-xs text-muted-foreground">{t("stt.modelHint")}</p>
          </div>

          <div className="grid gap-1.5">
            <Label htmlFor="sttLanguage">{t("stt.language")}</Label>
            <Input
              id="sttLanguage"
              value={draft.language ?? ""}
              placeholder="zh"
              onChange={(e) => update({ language: e.target.value })}
              className="text-base md:text-sm"
            />
            <p className="text-xs text-muted-foreground">{t("stt.languageHint")}</p>
          </div>
        </div>

        <div className="grid gap-1.5">
          <Label htmlFor="sttApiKey">{t("stt.apiKey")}</Label>
          <Input
            id="sttApiKey"
            type="password"
            value={draft.api_key ?? ""}
            disabled={keyMasked}
            readOnly={keyMasked}
            onChange={(e) => update({ api_key: e.target.value })}
            className="text-base md:text-sm"
          />
          {keyMasked ? (
            <p className="text-xs text-muted-foreground">{t("stt.apiKeyStored")}</p>
          ) : (
            <p className="text-xs text-muted-foreground">{t("stt.apiKeyHint", { envKey: "GOCLAW_AUDIO_STT_API_KEY" })}</p>
          )}
        </div>

        <div className="grid gap-1.5">
          <Label htmlFor="sttTimeout">{t("stt.timeout")}</Label>
          <Input
            id="sttTimeout"
            type="number"
            min={0}
            step={1000}
            value={draft.timeout_ms ?? ""}
            placeholder="30000"
            onChange={(e) => update({ timeout_ms: e.target.value === "" ? "" : Number(e.target.value) })}
            className="text-base md:text-sm"
          />
          <p className="text-xs text-muted-foreground">{t("stt.timeoutHint")}</p>
        </div>

        <p className="text-xs text-muted-foreground">{t("stt.restartNote")}</p>

        {dirty && (
          <div className="flex justify-end pt-1">
            <Button size="sm" onClick={handleSave} disabled={saving} className="gap-1.5">
              <Save className="h-3.5 w-3.5" /> {saving ? t("saving") : t("save")}
            </Button>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
