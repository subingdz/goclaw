import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import { AlertTriangle, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "@/components/ui/dialog";

// The dialog behind Built-in tools → stt → Settings.
//
// It used to carry an STT provider list, an ElevenLabs key and a proxy block, and none of that
// was ever read: the gateway builds its transcription chain from `audio.stt`, which the panel
// edits under Config → Integrations → Speech-to-Text. So those fields are gone, and what is left
// is the one setting that genuinely belongs to this tool — the per-message WhatsApp opt-in —
// plus a pointer to where the backend is actually configured.

interface Props {
  initialSettings: Record<string, unknown>;
  onSave: (settings: Record<string, unknown>) => Promise<void>;
  onCancel: () => void;
}

export function buildSttPayload(whatsappEnabled: boolean): Record<string, unknown> {
  return { whatsapp_enabled: whatsappEnabled };
}

export function SttProviderForm({ initialSettings, onSave, onCancel }: Props) {
  const { t } = useTranslation("tools");
  const [whatsappEnabled, setWhatsappEnabled] = useState(
    (initialSettings.whatsapp_enabled as boolean) ?? false,
  );
  const [saving, setSaving] = useState(false);

  const handleSave = async () => {
    setSaving(true);
    try {
      await onSave(buildSttPayload(whatsappEnabled));
    } catch {
      // toast shown by hook
    } finally {
      setSaving(false);
    }
  };

  return (
    <div>
      <DialogHeader>
        <DialogTitle>{t("builtin.sttForm.title")}</DialogTitle>
        <DialogDescription>{t("builtin.sttForm.description")}</DialogDescription>
      </DialogHeader>

      <div className="space-y-5 my-4">
        <div className="space-y-1.5 rounded-md border border-border bg-muted/40 px-3 py-2.5">
          <p className="text-sm text-muted-foreground">{t("builtin.sttForm.backendNote")}</p>
          <Link
            to="/config"
            className="text-sm font-medium text-primary underline-offset-4 hover:underline"
            data-testid="stt-backend-link"
          >
            {t("builtin.sttForm.backendLink")}
          </Link>
        </div>

        {/* WhatsApp toggle */}
        <div className="space-y-2">
          <div className="flex items-start gap-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-800 dark:border-amber-800 dark:bg-amber-950/30 dark:text-amber-300">
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
            <span data-testid="whatsapp-privacy-banner">{t("builtin.sttForm.whatsappPrivacyWarning")}</span>
          </div>
          <label className="flex items-center gap-2 cursor-pointer select-none">
            <input
              type="checkbox"
              checked={whatsappEnabled}
              onChange={(e) => setWhatsappEnabled(e.target.checked)}
              className="h-4 w-4"
            />
            <span className="text-base md:text-sm font-medium">{t("builtin.sttForm.whatsappEnabledLabel")}</span>
          </label>
        </div>
      </div>

      <DialogFooter>
        <Button variant="outline" onClick={onCancel}>
          {t("builtin.sttForm.cancel")}
        </Button>
        <Button onClick={handleSave} disabled={saving}>
          {saving && <Loader2 className="h-4 w-4 animate-spin" />}
          {saving ? t("builtin.sttForm.saving") : t("builtin.sttForm.save")}
        </Button>
      </DialogFooter>
    </div>
  );
}
