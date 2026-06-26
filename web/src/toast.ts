// Tiny toast service (RFC-013 polish). A pub/sub queue; <toast-host> (mounted in
// app-root) renders the active toasts. Any component calls toast(message, type)
// for a transient, non-blocking confirmation or error, instead of an inline alert.

import { ApiError } from "./api/client.js";

export type ToastType = "success" | "error" | "info";

export interface ToastItem {
  id: number;
  message: string;
  type: ToastType;
  // the request's trace id, shown on error toasts so a user or support can quote it
  // into the trace tooling (RFC-021 section 8).
  traceId?: string;
}

type Listener = (toasts: ToastItem[]) => void;

let toasts: ToastItem[] = [];
let nextId = 1;
const listeners = new Set<Listener>();

function emit(): void {
  for (const fn of listeners) fn(toasts);
}

export function toast(
  message: string,
  type: ToastType = "info",
  ms = 4000,
  traceId?: string,
): void {
  const id = nextId++;
  toasts = [...toasts, { id, message, type, ...(traceId ? { traceId } : {}) }];
  emit();
  setTimeout(() => dismissToast(id), ms);
}

// toastError shows an error toast from a thrown value: an ApiError's own message
// plus its trace id (so the id is on screen to copy, RFC-021 section 8), or the
// fallback message for any non-api error. It replaces the repeated
// `err instanceof ApiError ? err.message : t("state.error")` idiom at the call sites.
// Error toasts with a trace id linger a bit longer so there is time to copy it.
export function toastError(err: unknown, fallback: string): void {
  if (err instanceof ApiError) {
    toast(err.message, "error", 8000, err.traceId);
    return;
  }
  toast(fallback, "error");
}

export function dismissToast(id: number): void {
  toasts = toasts.filter((t) => t.id !== id);
  emit();
}

export function subscribeToasts(fn: Listener): () => void {
  listeners.add(fn);
  fn(toasts);
  return () => listeners.delete(fn);
}
