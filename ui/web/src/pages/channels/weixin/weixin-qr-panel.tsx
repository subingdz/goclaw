// Shared QR display for the WeChat create wizard and the re-auth dialog.
// One component because both show exactly the same thing, and the copy has to stay in step.

import { useTranslation } from "react-i18next";
import type { WeixinQrStatus } from "./use-weixin-qr-login";

interface WeixinQrPanelProps {
  status: WeixinQrStatus;
  qrPng: string | null;
  errorText: string;
}

export function WeixinQrPanel({ status, qrPng, errorText }: WeixinQrPanelProps) {
  const { t } = useTranslation("channels");

  return (
    <div className="flex flex-col items-center gap-4 py-4 min-h-[240px] min-w-0">
      {status === "idle" && (
        <p className="text-sm text-muted-foreground">{t("weixin.initializing")}</p>
      )}
      {status === "waiting" && !qrPng && (
        <p className="text-sm text-muted-foreground">{t("weixin.waitingForQr")}</p>
      )}
      {(status === "waiting" || status === "scaned") && qrPng && (
        <>
          <img
            src={`data:image/png;base64,${qrPng}`}
            alt={t("weixin.qrAlt")}
            className="w-52 h-52 border rounded bg-white"
          />
          <p className="text-xs text-muted-foreground text-center">
            {status === "scaned" ? t("weixin.scannedConfirm") : t("weixin.scanHint")}
          </p>
        </>
      )}
      {status === "error" && (
        <p className="text-sm text-destructive text-center px-2 break-words">{errorText}</p>
      )}
      {status === "done" && (
        <p className="text-sm text-green-600 font-medium">{t("weixin.loginSuccessLoading")}</p>
      )}
    </div>
  );
}
