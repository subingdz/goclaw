// WeChat wizard step: scan the iLink QR code to activate the instance.
// Registered in channel-wizard-registry.tsx.

import { useEffect } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { DialogFooter } from "@/components/ui/dialog";
import { WeixinQrPanel } from "./weixin-qr-panel";
import { resolveQrErrorText, useWeixinQrLogin } from "./use-weixin-qr-login";
import type { WizardAuthStepProps } from "../channel-wizard-registry";

export function WeixinAuthStep({ instanceId, onComplete, onSkip }: WizardAuthStepProps) {
  const { t } = useTranslation("channels");
  const { qrPng, status, errorMsg, errorReason, loading, start, retry, reset } =
    useWeixinQrLogin(instanceId);

  useEffect(() => {
    start();
    return () => reset();
  }, [start, reset]);

  useEffect(() => {
    if (status === "done") onComplete();
  }, [status, onComplete]);

  return (
    <>
      <WeixinQrPanel status={status} qrPng={qrPng} errorText={resolveQrErrorText(t, errorReason, errorMsg)} />
      <DialogFooter>
        <Button variant="outline" onClick={onSkip} disabled={loading}>
          {t("weixin.skip")}
        </Button>
        {status === "error" && (
          <Button onClick={() => retry()} disabled={loading}>
            {t("weixin.retry")}
          </Button>
        )}
      </DialogFooter>
    </>
  );
}
