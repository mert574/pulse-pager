// Tiny reactive holder for the current-user info. No store library. The cookie is
// httpOnly so JS never reads a token; "logged in" just means GET /api/v1/me
// returned 200. Components subscribe to be told when the session changes. The
// richer session+org+entitlements view lives in state/context.ts; this holds the
// raw identity the context derives from.

import type { Me } from "../api/types.js";

type Listener = () => void;

class SessionState {
  // null = unknown / logged out. Set after a successful /api/v1/me.
  private _me: Me | null = null;
  // false until the first /api/v1/me has resolved (success or 401), so views
  // can avoid flashing the login screen during the initial check.
  private _checked = false;
  private listeners = new Set<Listener>();

  get me(): Me | null {
    return this._me;
  }

  get checked(): boolean {
    return this._checked;
  }

  get isLoggedIn(): boolean {
    return this._me !== null;
  }

  setMe(me: Me | null): void {
    this._me = me;
    this._checked = true;
    this.notify();
  }

  // clear() is called by the api client on a 401 so the app drops back to the
  // logged-out view.
  clear(): void {
    this._me = null;
    this._checked = true;
    this.notify();
  }

  subscribe(fn: Listener): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  private notify(): void {
    for (const fn of this.listeners) fn();
  }
}

// One shared instance for the whole app.
export const session = new SessionState();
