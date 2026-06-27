/* Shared chrome for the docs site as two custom elements, so every page reuses
   the same nav and footer instead of repeating the markup.

     <pulse-header active="overview" base="./"></pulse-header>
     <pulse-footer base="./"></pulse-footer>

   active: which nav item to highlight (overview | api | auth | pricing).
   base:   path prefix to the site root from this page ("./" at the root,
           "../" for pages one folder deep like guides/). Defaults to "./".

   This file is loaded synchronously in <head> so the theme class lands on <html>
   before first paint (no flash), then the elements upgrade as the parser reaches
   them in <body>. The chrome is decorative/navigational; each page's real content
   (h1, copy) stays in static HTML. */
(function () {
  "use strict";

  /* Apply the saved theme as early as possible. With no saved choice we leave it
     to the OS preference, which the CSS handles via prefers-color-scheme. */
  var THEME_KEY = "pulse-theme";
  function savedTheme() {
    try { return localStorage.getItem(THEME_KEY); } catch (e) { return null; }
  }
  (function applyTheme() {
    var t = savedTheme();
    var root = document.documentElement;
    root.classList.remove("theme-dark", "theme-light");
    if (t === "dark") root.classList.add("theme-dark");
    else if (t === "light") root.classList.add("theme-light");
  })();

  function currentlyDark() {
    var t = savedTheme();
    if (t === "dark") return true;
    if (t === "light") return false;
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
  }

  function toggleTheme() {
    var next = currentlyDark() ? "light" : "dark";
    try { localStorage.setItem(THEME_KEY, next); } catch (e) {}
    var root = document.documentElement;
    root.classList.remove("theme-dark", "theme-light");
    root.classList.add(next === "dark" ? "theme-dark" : "theme-light");
  }

  /* The marquee phrases. Brand and value props only, no invented metrics. */
  var FOLIO = [
    "Pulse Pager",
    "Open-source uptime monitoring",
    "Know before your customers do",
    "Self-host free",
    "API-first",
    "Monitors · Incidents · Status pages",
    "Slack · Discord · Webhooks · PagerDuty"
  ];

  var GITHUB = "https://github.com/mert574/pulse-pager";
  var APP = "https://app.pulsepager.com";

  function folioTrack() {
    // Two identical runs back to back so the -50% marquee loops seamlessly.
    var run = FOLIO.map(function (p) { return "<b>" + p + "</b><i></i>"; }).join("");
    return '<div class="folio"><div class="track">' + run + run + "</div></div>";
  }

  function navLinks(base, active) {
    var items = [
      { key: "overview", label: "Overview", href: base },
      { key: "api", label: "API reference", href: base + "api.html" },
      { key: "auth", label: "Authentication", href: base + "guides/authentication.html" },
      { key: "pricing", label: "Pricing", href: base + "pricing.html" },
      { key: "github", label: "GitHub", href: GITHUB }
    ];
    return items.map(function (it) {
      var on = it.key === active ? ' class="on"' : "";
      return '<a' + on + ' href="' + it.href + '">' + it.label + "</a>";
    }).join("");
  }

  class PulseHeader extends HTMLElement {
    connectedCallback() {
      var base = this.getAttribute("base") || "./";
      var active = this.getAttribute("active") || "";
      this.innerHTML =
        folioTrack() +
        '<header class="topbar"><div class="topbar-inner wrap">' +
          '<a class="brand" href="' + base + '"><span class="sq"></span>Pulse Pager</a>' +
          '<input type="checkbox" id="pp-nav" class="navtoggle" hidden>' +
          '<nav class="topnav">' + navLinks(base, active) + "</nav>" +
          '<div class="topactions">' +
            '<button class="themebtn" type="button" aria-label="Toggle dark mode" data-theme-toggle>◑</button>' +
            '<a class="btn ghost" href="' + APP + '">Sign in</a>' +
            '<a class="btn" href="' + APP + '">Start free</a>' +
          "</div>" +
          '<label for="pp-nav" class="burger" aria-label="Toggle menu"><span></span><span></span><span></span></label>' +
        "</div></header>";

      var btn = this.querySelector("[data-theme-toggle]");
      if (btn) btn.addEventListener("click", toggleTheme);
    }
  }

  class PulseFooter extends HTMLElement {
    connectedCallback() {
      var base = this.getAttribute("base") || "./";
      this.innerHTML =
        '<footer class="colophon">' +
          '<div class="cmark">Pulse Pager</div>' +
          '<div class="cgrid">' +
            '<div class="ccol"><h4>Product</h4>' +
              '<a href="' + base + '">Overview</a>' +
              '<a href="' + base + 'pricing.html">Pricing</a>' +
              '<a href="' + APP + '">Sign in</a>' +
              '<a href="' + APP + '">Start free</a>' +
            "</div>" +
            '<div class="ccol"><h4>Developers</h4>' +
              '<a href="' + base + 'api.html">API reference</a>' +
              '<a href="' + base + 'guides/authentication.html">Authentication</a>' +
              '<a href="' + base + 'guides/authentication.html#webhooks">Webhooks</a>' +
            "</div>" +
            '<div class="ccol"><h4>Project</h4>' +
              '<a href="' + GITHUB + '">GitHub</a>' +
              '<a href="' + GITHUB + '">Self-hosting</a>' +
              '<a href="' + base + 'pricing.html">Cloud pricing</a>' +
            "</div>" +
            '<div class="ccol"><h4>Legal</h4>' +
              '<a href="' + base + 'terms.html">Terms</a>' +
              '<a href="' + base + 'privacy.html">Privacy</a>' +
              '<a href="' + base + 'refund.html">Refunds</a>' +
              '<a href="mailto:hi@pulsepager.com">Contact</a>' +
            "</div>" +
          "</div>" +
          '<div class="baseline">' +
            "<span>© 2026 Pulse Pager · Elastic License 2.0</span>" +
            '<span class="copy"><a href="https://pulsepager.com">pulsepager.com</a><span class="end"></span></span>' +
          "</div>" +
        "</footer>";
    }
  }

  customElements.define("pulse-header", PulseHeader);
  customElements.define("pulse-footer", PulseFooter);
})();
