import { html } from "lit";
import { customElement, state } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { navigate } from "../router.js";
import { spinner } from "./ui.js";
import { t, tDynamic } from "../i18n.js";

// /checkout is set as Paddle's default payment link, so the checkout redirect lands
// here as /checkout?_ptxn=<txn>. We load Paddle.js, call Initialize with the public
// client-side token, and Paddle.js reads _ptxn and opens the checkout overlay for that
// transaction (https://developer.paddle.com/build/transactions/default-payment-link).
//
// This page is just the overlay host: the real plan change happens later, when Paddle
// posts transaction.completed to the billing webhook and that reconciles the org plan.
// On completion (or if the user closes the overlay) we send them back to where they
// started checkout, stashed by billing-view before the redirect.

const PADDLE_JS = "https://cdn.paddle.com/paddle/v2/paddle.js";

// where billing-view parks the path to return to after the overlay closes.
const RETURN_KEY = "pulse.checkout.return";

// the public client-side token (not the API key), baked in at build time.
const CLIENT_TOKEN = (import.meta.env as Record<string, string | undefined>)
  .VITE_PADDLE_CLIENT_TOKEN;

interface PaddleGlobal {
  Environment?: { set(env: "production" | "sandbox"): void };
  Initialize(opts: {
    token: string;
    eventCallback?: (event: { name?: string }) => void;
  }): void;
}

declare global {
  interface Window {
    Paddle?: PaddleGlobal;
  }
}

type Status = "opening" | "done" | "none" | "unconfigured" | "error";

@customElement("checkout-view")
export class CheckoutView extends AppElement {
  @state() private status: Status = "opening";

  // guards against re-running the load if the element reconnects.
  private started = false;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.started) return;
    this.started = true;
    this.begin();
  }

  private begin(): void {
    const hasTxn = new URLSearchParams(window.location.search).has("_ptxn");
    if (!hasTxn) {
      this.status = "none";
      return;
    }
    if (!CLIENT_TOKEN) {
      this.status = "unconfigured";
      return;
    }
    this.loadPaddle();
  }

  private loadPaddle(): void {
    if (window.Paddle) {
      this.initPaddle();
      return;
    }
    const s = document.createElement("script");
    s.src = PADDLE_JS;
    s.onload = () => this.initPaddle();
    s.onerror = () => {
      this.status = "error";
    };
    document.head.appendChild(s);
  }

  private initPaddle(): void {
    const paddle = window.Paddle;
    if (!paddle) {
      this.status = "error";
      return;
    }
    // Initialize with the token; Paddle.js sees _ptxn in the URL and opens the
    // overlay. The callback lets us return the user to the app afterward.
    paddle.Initialize({
      token: CLIENT_TOKEN as string,
      eventCallback: (event) => {
        if (event.name === "checkout.completed") {
          this.status = "done";
          // give Paddle's own success screen a moment before we navigate away.
          window.setTimeout(() => navigate(this.returnPath()), 1500);
        } else if (event.name === "checkout.closed") {
          navigate(this.returnPath());
        }
      },
    });
  }

  // the path billing-view stashed before redirecting to Paddle, else the account
  // page. Read once and cleared so a later direct visit does not reuse it.
  private returnPath(): string {
    let path: string | null = null;
    try {
      path = window.sessionStorage.getItem(RETURN_KEY);
      window.sessionStorage.removeItem(RETURN_KEY);
    } catch {
      // sessionStorage can throw in locked-down browsers; fall through.
    }
    return path || "/account";
  }

  override render() {
    if (this.status === "none") return this.message(t("checkout.none"), true);
    if (this.status === "unconfigured")
      return this.message(t("checkout.unavailable"), true);
    if (this.status === "error")
      return this.message(t("checkout.unavailable"), true);
    if (this.status === "done") return this.message(t("checkout.done"), false);
    return this.message(t("checkout.opening"), false);
  }

  // a centered card behind the Paddle overlay. showReturn adds a link back into the
  // app for the states where no overlay will appear (no txn / not configured / error).
  private message(text: string, showReturn: boolean) {
    return html`
      <div class="grid min-h-[60vh] place-items-center p-6">
        <div
          class="border border-hair bg-bg p-8 max-w-sm w-full flex flex-col items-center text-center gap-4"
        >
          <span class="pulse-label">${tDynamic("checkout.kicker", "Secure checkout")}</span>
          ${this.status === "opening"
            ? html`<span class="text-brand" aria-hidden="true">${spinner()}</span>`
            : null}
          <p
            class="font-disp font-extrabold text-[20px] leading-tight tracking-[-0.02em] text-ink"
          >
            ${text}
          </p>
          ${showReturn
            ? html`<a class="pulse-btn mt-1" href="/account"
                >${t("checkout.return")}</a
              >`
            : null}
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "checkout-view": CheckoutView;
  }
}
