// Re-activation dialog for WeChat — scan a fresh iLink QR code onto an existing instance.
// Registered in channel-wizard-registry.tsx.

import { useEffect } from "react";
import { useTranslation } from "react-i18next";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { WeixinQrPanel } from "./weixin-qr-panel";
import { resolveQrErrorText, useWeixinQrLogin } from "./use-weixin-qr-login";
import type { ReauthDialogProps } from "../channel-wizard-registry";

export function WeixinReauthDialog({
  open,
  onOpenChange,
  instanceId,
  instanceName,
  onSuccess,
}: ReauthDialogProps) {
  const { t } = useTranslation("channels");
  const { qrPng, status, errorMsg, errorReason, loading, start, reset, retry } =
    useWeixinQrLogin(instanceId);

  // Trigger only on the open/close transition: `start` changes identity with the instance id
  // and re-running it would replace the code the user is already pointing a phone at.
  useEffect(() => {
    if (open) start();
  }, [open]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (!open) reset();
  }, [open, reset]);

  useEffect(() => {
    if (status !== "done") return;
    onSuccess();
    const id = setTimeout(() => onOpenChange(false), 1500);
    return () => clearTimeout(id);
  }, [status, onSuccess, onOpenChange]);

  return (
    <Dialog open={open} onOpenChange={(v) => { if (!loading) onOpenChange(v); }}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>{t("weixin.reauthTitle", { name: instanceName })}</DialogTitle>
          <DialogDescription>
            {status === "done" ? t("weixin.connectedSuccess") : t("weixin.replaceTokenWarning")}
          </DialogDescription>
        </DialogHeader>

        <WeixinQrPanel status={status} qrPng={qrPng} errorText={resolveQrErrorText(t, errorReason, errorMsg)} />

        <div className="flex justify-end gap-2">
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={loading && status !== "done"}>
            {t("weixin.close")}
          </Button>
          {status === "error" && (
            <Button onClick={() => retry()} disabled={loading}>
              {t("weixin.retry")}
            </Button>
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}
