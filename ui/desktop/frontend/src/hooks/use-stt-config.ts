import { useCallback, useEffect, useState } from 'react'
import { getWsClient } from '../lib/ws'

// Desktop speech-to-text configuration.
//
// This edits `audio.stt` — the surface the gateway actually reads when it builds the STT chain
// at startup — over the same WS methods the web panel uses. It is not the per-tool settings blob:
// the `stt` builtin tool's own settings carry a single key that the chain consults per message
// (whatsapp_enabled), and saving anything else there does nothing, which is what the earlier
// version of this dialog did until it was retargeted.
//
// There is no REST counterpart: config.get/config.patch exist only as WS methods, and the desktop
// build connects as the owner of its own single-tenant gateway, so it passes the scope guards.

export type SttProviderName = 'openai_compat' | 'elevenlabs' | 'proxy'

export interface SttConfig {
  provider: string
  base_url: string
  model: string
  language: string
  fallback: string
  timeout_ms: number
  silk_decoder: string
  // api_key arrives masked ("***") from config.get. It is echoed back only when the operator
  // typed a new value; sending the mask would store the mask.
  api_key: string
}

export const emptySttConfig = (): SttConfig => ({
  provider: '',
  base_url: '',
  model: '',
  language: '',
  fallback: '',
  timeout_ms: 0,
  silk_decoder: '',
  api_key: '',
})

interface ConfigGetResponse {
  config?: { audio?: { stt?: Partial<SttConfig> } }
  hash?: string
}

const MASK = '***'

export function isMaskedSecret(value: string): boolean {
  return value.includes(MASK)
}

export function useDesktopSttConfig() {
  const [draft, setDraft] = useState<SttConfig>(emptySttConfig())
  const [baseHash, setBaseHash] = useState('')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    try {
      const res = (await getWsClient().call('config.get')) as ConfigGetResponse
      const stt = res.config?.audio?.stt ?? {}
      setDraft({ ...emptySttConfig(), ...stt, timeout_ms: stt.timeout_ms ?? 0 })
      setBaseHash(res.hash ?? '')
      setError('')
    } catch (err) {
      // Not connected yet is a normal first render, not a failure worth blocking the dialog on.
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
    let unsub: (() => void) | undefined
    try {
      unsub = getWsClient().on('config.updated', () => void load())
    } catch {
      /* ws not ready */
    }
    return () => unsub?.()
  }, [load])

  // save patches only audio.stt. Empty strings are sent on purpose: the merge replaces the key,
  // and the writer drops empty values, so clearing a field in the dialog clears it on disk.
  const save = useCallback(
    async (next: SttConfig) => {
      const payload: Partial<SttConfig> = {
        provider: next.provider.trim(),
        base_url: next.base_url.trim(),
        model: next.model.trim(),
        language: next.language.trim(),
        fallback: next.fallback.trim(),
        timeout_ms: next.timeout_ms > 0 ? next.timeout_ms : 0,
        silk_decoder: next.silk_decoder.trim(),
      }
      if (next.api_key && !isMaskedSecret(next.api_key)) {
        payload.api_key = next.api_key.trim()
      }

      setSaving(true)
      setError('')
      try {
        await getWsClient().call('config.patch', {
          raw: JSON.stringify({ audio: { stt: payload } }),
          baseHash,
        })
        setDraft(next)
        return true
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err))
        return false
      } finally {
        setSaving(false)
      }
    },
    [baseHash],
  )

  return { draft, setDraft, loading, saving, error, save, reload: load }
}
