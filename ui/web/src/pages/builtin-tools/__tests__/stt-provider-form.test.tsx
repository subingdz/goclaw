/**
 * Unit tests for the stt tool dialog's remaining contract.
 *
 * NOTE: @testing-library/react is not installed in ui/web — tests cover pure logic and the
 * locale contract rather than DOM rendering (mirrors the desktop form's earlier pattern).
 */
import { describe, it, expect } from "vitest";
import { buildSttPayload } from "../stt-provider-form";

const DEAD_KEYS = [
  "providersLabel",
  "elevenlabsSectionTitle",
  "elevenlabsApiKeyLabel",
  "elevenlabsDefaultLanguageLabel",
  "proxySectionTitle",
  "proxyUrlLabel",
  "proxyApiKeyLabel",
  "proxyTenantIdLabel",
  "providersRequiredError",
];

// The payload shape is the whole point of the change: this tool owns exactly one setting, and the
// gateway reads exactly that key out of it.
describe("buildSttPayload", () => {
  it("emits only whatsapp_enabled", () => {
    expect(buildSttPayload(false)).toEqual({ whatsapp_enabled: false });
    expect(buildSttPayload(true)).toEqual({ whatsapp_enabled: true });
  });

  it("carries none of the provider/elevenlabs/proxy keys the dialog used to save", () => {
    const payload = buildSttPayload(true);
    for (const dead of ["providers", "elevenlabs", "proxy"]) {
      expect(payload).not.toHaveProperty(dead);
    }
  });
});

describe("sttForm locale contract", () => {
  const locales = ["en", "vi", "zh", "ru"] as const;

  it.each(locales)(" %s keeps the live keys and drops the dead ones", async (loc) => {
    const mod = await import(`@/i18n/locales/${loc}/tools.json`);
    const form = (mod as unknown as { builtin: { sttForm: Record<string, string> } })
      .builtin.sttForm;

    expect(form.whatsappPrivacyWarning).toContain("WhatsApp");
    expect(form.whatsappEnabledLabel).toBeTruthy();
    // The pointer to where the backend actually lives is the replacement for the removed fields.
    expect(form.backendNote).toBeTruthy();
    expect(form.backendLink).toBeTruthy();

    for (const dead of DEAD_KEYS) {
      expect(form).not.toHaveProperty(dead);
    }
  });

  it("ko falls back rather than shipping a half-translated dialog", async () => {
    const ko = await import("@/i18n/locales/ko/tools.json");
    const form = (ko as unknown as { builtin?: { sttForm?: Record<string, string> } }).builtin?.sttForm;
    // Either absent (i18next falls back to en) or complete — never a stale subset that still
    // advertises fields the dialog no longer renders.
    if (form) {
      for (const dead of DEAD_KEYS) expect(form).not.toHaveProperty(dead);
    }
  });

  // The WhatsApp warning is the one string the surviving control actually shows, so its presence
  // in every shipped locale is a real contract — the dialog cannot explain the opt-in without it.
  it.each(["en", "vi", "zh", "ru"])("%s keeps whatsappPrivacyWarning", async (locale) => {
    const messages = await import(`@/i18n/locales/${locale}/tools.json`);
    const warning = (
      messages as unknown as { builtin?: { sttForm?: { whatsappPrivacyWarning?: string } } }
    ).builtin?.sttForm?.whatsappPrivacyWarning;
    expect(warning).toBeTruthy();
  });
});
