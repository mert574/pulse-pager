// Shared error handling for the check-now action (RFC-013 multi-region). Both the
// monitors list and the monitor detail call checkNow and want the same toasts on
// the failure paths, so the mapping lives here once.

import { ApiError } from "./api/client.js";
import { t, tDynamic } from "./i18n.js";
import { toast } from "./toast.js";

// Turn a checkNow rejection into a toast. 409 splits on the error code: a
// disabled monitor reads differently from an already-running check. 429 shows a
// countdown from retry_after (the Error.fields the server sends) and adds an
// upgrade hint when the plan can go higher.
export function toastCheckError(err: unknown): void {
  if (!(err instanceof ApiError)) {
    toast(t("state.error"), "error");
    return;
  }
  if (err.status === 409) {
    toast(
      err.code === "monitor_disabled"
        ? t("monitor.checkDisabled")
        : t("monitor.checkConflict"),
      "error",
    );
    return;
  }
  if (err.status === 429) {
    const seconds = retryAfterSeconds(err);
    const upgrade = err.fields?.upgrade;
    const key = upgrade ? "monitor.checkRateLimitedUpgrade" : "monitor.checkRateLimited";
    toast(tDynamic(key, "", { seconds }), "error");
    return;
  }
  toast(err.message, "error");
}

// Seconds to wait before retrying, read from Error.fields.retry_after (a string
// the server fills in alongside the Retry-After header). Falls back to 0 when
// missing or unparseable, so the message still renders.
function retryAfterSeconds(err: ApiError): number {
  const raw = err.fields?.retry_after;
  const n = raw != null ? Number.parseInt(raw, 10) : NaN;
  return Number.isFinite(n) && n > 0 ? n : 0;
}
