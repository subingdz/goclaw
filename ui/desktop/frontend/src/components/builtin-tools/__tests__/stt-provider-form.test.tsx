import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { I18nextProvider } from 'react-i18next'
import i18n from '../../../i18n'
import { SttProviderForm } from '../stt-provider-form'

// The dialog must edit audio.stt — the surface the gateway reads when it builds the STT chain —
// and not the tool settings blob, which used to hold a provider list and two API keys that no
// code ever consulted. whatsapp_enabled is the one key that still belongs to the tool.

interface PatchCall {
  method: string
  params?: Record<string, unknown>
}

let calls: PatchCall[] = []
let configResponse: unknown = {
  config: {
    audio: {
      stt: {
        provider: 'openai_compat',
        base_url: 'https://api.siliconflow.cn/v1',
        model: 'FunAudioLLM/SenseVoiceSmall',
        language: 'vi',
        fallback: '',
        timeout_ms: 30000,
        silk_decoder: '',
        api_key: '***abc123',
      },
    },
  },
  hash: 'hash-1',
}

vi.mock('../../../lib/ws', () => ({
  getWsClient: () => ({
    call: vi.fn(async (method: string, params?: Record<string, unknown>) => {
      calls.push({ method, params })
      if (method === 'config.get') return configResponse
      return { ok: true }
    }),
    on: vi.fn(() => () => {}),
  }),
}))

function renderForm(
  onSave = vi.fn().mockResolvedValue(undefined),
  initialSettings: Record<string, unknown> = {},
) {
  // The suite runs with the browser's locale otherwise, which makes the button names move
  // between languages; these tests click by label.
  i18n.changeLanguage('en')
  return render(
    <I18nextProvider i18n={i18n}>
      <SttProviderForm
        initialSettings={initialSettings}
        onSave={onSave}
        onCancel={vi.fn()}
      />
    </I18nextProvider>,
  );
}

function lastPatch(): Record<string, unknown> | undefined {
  const patch = [...calls].reverse().find((c) => c.method === 'config.patch')
  return patch?.params
}

function patchedStt(params?: Record<string, unknown>) {
  const raw = JSON.parse((params?.raw as string) ?? '{}')
  return raw?.audio?.stt as Record<string, unknown>
}

describe('SttProviderForm (desktop, audio.stt)', () => {
  beforeEach(() => {
    calls = []
    configResponse = {
      config: {
        audio: {
          stt: {
            provider: 'openai_compat',
            base_url: 'https://api.siliconflow.cn/v1',
            model: 'FunAudioLLM/SenseVoiceSmall',
            language: 'vi',
            fallback: '',
            timeout_ms: 30000,
            silk_decoder: '',
            api_key: '***abc123',
          },
        },
      },
      hash: 'hash-1',
    }
    vi.clearAllMocks()
  })

  it('loads the real audio.stt values into the form', async () => {
    renderForm()
    await waitFor(() => expect(calls.some((c) => c.method === 'config.get')).toBe(true))

    const provider = (await screen.findByTestId('stt-provider')) as HTMLSelectElement
    const baseUrl = (await screen.findByTestId('stt-base-url')) as HTMLInputElement
    expect(provider.value).toBe('openai_compat')
    expect(baseUrl.value).toBe('https://api.siliconflow.cn/v1')
  })

  it('drops the dead fields: no provider checkboxes, no proxy tenant id', async () => {
    renderForm()
    await screen.findByTestId('stt-base-url')
    expect(screen.queryByPlaceholderText('xi-...')).toBeNull()
    expect(screen.queryByPlaceholderText('https://...')).toBeNull()
    expect(screen.queryByLabelText(/tenant id/i)).toBeNull()
  })

  it('saves audio.stt through config.patch and only whatsapp_enabled through the tool settings', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined)
    renderForm(onSave, { whatsapp_enabled: false, providers: ['elevenlabs'] })
    await screen.findByTestId('stt-base-url')

    fireEvent.click(screen.getByRole('checkbox', { name: /whatsapp/i }))
    fireEvent.click(screen.getByRole('button', { name: /save|保存|сохранить/i }))

    await waitFor(() => expect(onSave).toHaveBeenCalled())
    const payload = lastPatch()
    expect(payload).toBeTruthy()
    expect(payload?.baseHash).toBe('hash-1')
    expect(patchedStt(payload).provider).toBe('openai_compat')
    expect(patchedStt(payload).timeout_ms).toBe(30000)

    // The masked key must never be echoed back: writing the mask would store it as the key.
    expect(patchedStt(payload).api_key).toBeUndefined()

    // The tool settings keep exactly the one key the chain consults per message.
    expect(onSave).toHaveBeenCalledWith(
      expect.not.objectContaining({ providers: expect.anything() }),
    )
    expect(onSave.mock.calls[0][0]).toEqual({ whatsapp_enabled: true })
  })

  it('sends a typed API key, and refuses a provider without a base URL', async () => {
    renderForm()
    await screen.findByTestId('stt-base-url')

    const baseUrl = (await screen.findByTestId('stt-base-url')) as HTMLInputElement
    fireEvent.change(baseUrl, { target: { value: '' } })
    fireEvent.click(screen.getByRole('button', { name: /save|保存/i }))

    expect(await screen.findByTestId('stt-form-error')).toBeTruthy()
    expect(calls.some((c) => c.method === 'config.patch')).toBe(false)

    fireEvent.change(baseUrl, { target: { value: 'https://api.siliconflow.cn/v1' } })
    const apiKey = (await screen.findByTestId('stt-api-key')) as HTMLInputElement
    fireEvent.change(apiKey, { target: { value: 'sk-new' } })
    fireEvent.click(screen.getByRole('button', { name: /save|保存/i }))

    await waitFor(() => expect(calls.some((c) => c.method === 'config.patch')).toBe(true))
    expect(patchedStt(lastPatch()).api_key).toBe('sk-new')
  })
})
