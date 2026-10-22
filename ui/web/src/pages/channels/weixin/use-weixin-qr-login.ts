import { useCallback, useState } from "react";
import { useWsCall } from "@/hooks/use-ws-call";
import { useWsEvent } from "@/hooks/use-ws-event";

export type WeixinQrStatus = "idle" | "waiting" | "scaned" | "done" | "error";

interface QrState {
  qrPng: string | null;
  status: WeixinQrStatus;
  errorMsg: string;
  /** Stable code from the gateway, so the panel localizes the failure itself. */
  errorReason: string;
}

const IDLE: QrState = { qrPng: null, status: "idle", errorMsg: "", errorReason: "" };

/**
 * Resolve a QR failure into display text. The gateway sends a reason code for anything the
 * panel can name itself and raw detail only where the code cannot say enough (a refused
 * host, a failing endpoint), so the detail is appended rather than chosen instead.
 */
export function resolveQrErrorText(
  t: (key: string) => string,
  reason: string,
  serverMessage: string,
): string {
  const key = reason ? `weixin.qrReason.${reason}` : "";
  const localized = key ? t(key) : "";
  // i18next echoes the key back when a translation is missing.
  const known = !!localized && localized !== key;
  if (!known) return serverMessage || t("weixin.qrReason.generic");
  return serverMessage ? `${localized} — ${serverMessage}` : localized;
}

export function useWeixinQrLogin(instanceId: string | null) {
  const [state, setState] = useState<QrState>(IDLE);
  const { call: startQR, loading } = useWsCall("weixin.qr.start");

  const start = useCallback(async () => {
    if (!instanceId) return;
    setState({ qrPng: null, status: "waiting", errorMsg: "", errorReason: "" });
    try {
      await startQR({ instance_id: instanceId });
    } catch (err) {
      setState({
        qrPng: null,
        status: "error",
        errorMsg: err instanceof Error ? err.message : String(err),
        errorReason: "start",
      });
    }
  }, [startQR, instanceId]);

  const reset = useCallback(() => setState(IDLE), []);

  useWsEvent(
    "weixin.qr.code",
    useCallback((payload: unknown) => {
      const p = payload as { instance_id: string; png_b64: string };
      if (p.instance_id !== instanceId) return;
      setState((prev) => ({ ...prev, qrPng: p.png_b64, status: "waiting", errorMsg: "", errorReason: "" }));
    }, [instanceId]),
  );

  useWsEvent(
    "weixin.qr.status",
    useCallback((payload: unknown) => {
      const p = payload as { instance_id: string; status: string };
      if (p.instance_id !== instanceId) return;
      if (p.status !== "scaned") return;
      setState((prev) => (prev.status === "waiting" ? { ...prev, status: "scaned" } : prev));
    }, [instanceId]),
  );

  useWsEvent(
    "weixin.qr.done",
    useCallback((payload: unknown) => {
      const p = payload as {
        instance_id: string;
        success: boolean;
        reason?: string;
        error?: string;
      };
      if (p.instance_id !== instanceId) return;
      if (p.success) {
        setState((prev) => ({ ...prev, status: "done", errorMsg: "", errorReason: "" }));
        return;
      }
      setState((prev) => ({
        ...prev,
        status: "error",
        errorMsg: p.error ?? "",
        errorReason: p.reason ?? "error",
      }));
    }, [instanceId]),
  );

  return {
    qrPng: state.qrPng,
    status: state.status,
    errorMsg: state.errorMsg,
    errorReason: state.errorReason,
    loading,
    start,
    reset,
    retry: start,
  };
}
