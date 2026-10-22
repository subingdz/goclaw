import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useDesktopSttConfig, isMaskedSecret, type SttProviderName } from '../../hooks/use-stt-config'

// The dialog behind Built-in tools → stt → Settings.
//
// It used to edit the tool's own settings blob (provider list, ElevenLabs key, proxy URL) and
// nothing in the gateway read any of that except whatsapp_enabled: voice was transcribed according
// to `audio.stt`, which this dialog never touched. So the form is now the editor for `audio.stt`
// itself over config.get/config.patch, and keeps the one setting that genuinely lives on the tool.

const PROVIDERS: SttProviderName[] = ['openai_compat', 'elevenlabs', 'proxy']

const inputCls =
  'w-full bg-surface-tertiary border border-border rounded-lg px-2.5 py-1.5 text-sm text-text-primary placeholder:text-text-muted focus:outline-none focus:ring-1 focus:ring-accent'

interface Props {
  initialSettings: Record<string, unknown>
  onSave: (settings: Record<string, unknown>) => Promise<void>
  onCancel: () => void
}

export function SttProviderForm({ initialSettings, onSave, onCancel }: Props) {
  const { t } = useTranslation('tools')
  const { draft, setDraft, loading, saving, error, save } = useDesktopSttConfig()
  const [whatsappEnabled, setWhatsappEnabled] = useState(
    (initialSettings.whatsapp_enabled as boolean) ?? false,
  )
  const [formError, setFormError] = useState('')
  const [saved, setSaved] = useState(false)

  useEffect(() => {
    // A (re)load clears the previous validation complaint; fetch failures stay as the hook's own
    // error and are rendered below.
    setFormError('')
  }, [loading])

  const set = (patch: Partial<typeof draft>) => setDraft((prev) => ({ ...prev, ...patch }))

  const handleSave = async () => {
    // A provider chosen without an endpoint is how a gateway ends up with an empty chain and
    // voice messages that quietly stay untranscribed. Say it before the save, not after.
    if (draft.provider && !draft.base_url.trim()) {
      setFormError(t('builtin.sttForm.baseUrlRequiredError'))
      return
    }
    setFormError('')
    if (!(await save(draft))) return
    try {
      await onSave({ whatsapp_enabled: whatsappEnabled })
      setSaved(true)
    } catch {
      // The parent surfaces a tool-setting failure.
    }
  }

  return (
    <div className="flex flex-col overflow-hidden">
      <div className="flex-1 overflow-y-auto px-5 py-4 space-y-5">
        <p className="text-xs text-text-muted">{t('builtin.sttForm.description')}</p>

        {loading && <p className="text-xs text-text-muted">{t('builtin.sttForm.loading')}</p>}

        <div className="space-y-1">
          <label className="text-xs text-text-secondary">{t('builtin.sttForm.providerLabel')}</label>
          <select
            value={draft.provider}
            onChange={(e) => set({ provider: e.target.value })}
            className={inputCls}
            data-testid="stt-provider"
          >
            <option value="">{t('builtin.sttForm.providerNone')}</option>
            {PROVIDERS.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </select>
        </div>

        <div className="space-y-1">
          <label className="text-xs text-text-secondary">{t('builtin.sttForm.baseUrlLabel')}</label>
          <input
            type="text"
            value={draft.base_url}
            onChange={(e) => set({ base_url: e.target.value })}
            placeholder="https://api.siliconflow.cn/v1"
            className={inputCls}
            data-testid="stt-base-url"
          />
          <p className="text-[11px] text-text-muted">{t('builtin.sttForm.baseUrlHint')}</p>
        </div>

        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div className="space-y-1">
            <label className="text-xs text-text-secondary">{t('builtin.sttForm.modelLabel')}</label>
            <input
              type="text"
              value={draft.model}
              onChange={(e) => set({ model: e.target.value })}
              placeholder="FunAudioLLM/SenseVoiceSmall"
              className={inputCls}
            />
          </div>
          <div className="space-y-1">
            <label className="text-xs text-text-secondary">{t('builtin.sttForm.languageLabel')}</label>
            <input
              type="text"
              value={draft.language}
              onChange={(e) => set({ language: e.target.value })}
              placeholder="vi"
              className={inputCls}
            />
          </div>
          <div className="space-y-1">
            <label className="text-xs text-text-secondary">{t('builtin.sttForm.fallbackLabel')}</label>
            <input
              type="text"
              value={draft.fallback}
              onChange={(e) => set({ fallback: e.target.value })}
              placeholder="proxy"
              className={inputCls}
            />
          </div>
          <div className="space-y-1">
            <label className="text-xs text-text-secondary">{t('builtin.sttForm.timeoutLabel')}</label>
            <input
              type="number"
              min={0}
              step={1000}
              value={draft.timeout_ms || 0}
              onChange={(e) => set({ timeout_ms: Number(e.target.value) })}
              className={inputCls}
            />
            <p className="text-[11px] text-text-muted">{t('builtin.sttForm.timeoutHint')}</p>
          </div>
        </div>

        <div className="space-y-1">
          <label className="text-xs text-text-secondary">{t('builtin.sttForm.apiKeyLabel')}</label>
          <input
            type="password"
            value={draft.api_key}
            onChange={(e) => set({ api_key: e.target.value })}
            placeholder={isMaskedSecret(draft.api_key) ? t('builtin.sttForm.apiKeySet') : 'sk-...'}
            className={inputCls}
            data-testid="stt-api-key"
          />
          <p className="text-[11px] text-text-muted">{t('builtin.sttForm.apiKeyHint')}</p>
        </div>

        <div className="space-y-1">
          <label className="text-xs text-text-secondary">{t('builtin.sttForm.silkDecoderLabel')}</label>
          <input
            type="text"
            value={draft.silk_decoder}
            onChange={(e) => set({ silk_decoder: e.target.value })}
            placeholder="/usr/local/bin/silk_v3_decoder"
            className={inputCls}
          />
          <p className="text-[11px] text-text-muted">{t('builtin.sttForm.silkDecoderHint')}</p>
        </div>

        <p className="text-[11px] text-text-muted">{t('builtin.sttForm.restartNote')}</p>

        {/* WhatsApp section — privacy banner ABOVE the toggle */}
        <div className="space-y-2">
          <div
            className="flex items-start gap-2 rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-800"
            data-testid="whatsapp-privacy-banner"
          >
            <svg className="mt-0.5 h-3.5 w-3.5 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2}>
              <path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
              <line x1="12" y1="9" x2="12" y2="13" />
              <line x1="12" y1="17" x2="12.01" y2="17" />
            </svg>
            <span>{t('builtin.sttForm.whatsappPrivacyWarning')}</span>
          </div>
          <label className="flex items-center gap-2 cursor-pointer select-none text-sm">
            <input
              type="checkbox"
              checked={whatsappEnabled}
              onChange={(e) => setWhatsappEnabled(e.target.checked)}
              className="h-4 w-4 rounded"
            />
            <span className="font-medium">{t('builtin.sttForm.whatsappEnabledLabel')}</span>
          </label>
        </div>

        {(error || formError) && (
          <p className="text-xs text-red-500" data-testid="stt-form-error">
            {formError || error}
          </p>
        )}
        {saved && (
          <p className="text-xs text-green-600" data-testid="stt-form-saved">
            {t('builtin.sttForm.saved')}
          </p>
        )}
      </div>

      {/* Footer */}
      <div className="flex items-center justify-end gap-2 border-t border-border px-5 py-3 bg-surface-secondary">
        <button
          type="button"
          onClick={onCancel}
          className="px-3 py-1.5 text-sm rounded-lg border border-border text-text-secondary hover:text-text-primary"
        >
          {t('builtin.sttForm.cancel')}
        </button>
        <button
          type="button"
          onClick={handleSave}
          disabled={saving || loading}
          className="px-3 py-1.5 text-sm rounded-lg bg-accent text-white disabled:opacity-50"
        >
          {saving ? t('builtin.sttForm.saving') : t('builtin.sttForm.save')}
        </button>
      </div>
    </div>
  )
}
