// Tiny toast service (RFC-013 polish). A pub/sub queue; <toast-host> (mounted in
// app-root) renders the active toasts. Any component calls toast(message, type)
// for a transient, non-blocking confirmation or error, instead of an inline alert.

export type ToastType = "success" | "error" | "info";

export interface ToastItem {
  id: number;
  message: string;
  type: ToastType;
}

type Listener = (toasts: ToastItem[]) => void;

let toasts: ToastItem[] = [];
let nextId = 1;
const listeners = new Set<Listener>();

function emit(): void {
  for (const fn of listeners) fn(toasts);
}

export function toast(message: string, type: ToastType = "info", ms = 4000): void {
  const id = nextId++;
  toasts = [...toasts, { id, message, type }];
  emit();
  setTimeout(() => dismissToast(id), ms);
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
