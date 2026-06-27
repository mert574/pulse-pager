# Competitive pricing analysis

Date: 2026-06-22. Source: deep-research sweep of public competitor pricing pages
(17 sources fetched, 25 claims verified with 3-vote adversarial checking, 4 killed).
Prices are a mid-2026 snapshot and change often, so re-verify against live pages
before any public commitment.

## Competitor landscape (USD)

| Competitor | Free tier | Cheapest paid | Entry "pro" | Top / enterprise | Priced by |
|---|---|---|---|---|---|
| UptimeRobot | 50 monitors, 5-min, SSL/DNS/keyword, 3-mo retention, no API | Solo ~$10/mo ($9 annual): 60s, 12-mo retention, API | Team ~$38/mo ($33 annual): 100 monitors, 60s, custom-domain status pages, 24-mo retention | Enterprise: 30s, 200-1000+ monitors | monitors + interval |
| Better Stack | 10 monitors + 10 heartbeats + 1 status page | 50-monitor pack $25/mo | a-la-carte stacks (~$25-46) | quote | a-la-carte packs |
| Checkly | limited | Starter $24/mo | Team $64/mo | quote | usage (check runs) + seats |
| Hyperping | none | Essentials $24/mo: 50 monitors, 30s, 2 seats, 7-day | Pro $74/mo: 100 monitors, 30s, 5 seats, 10 browser checks, 14-day | quote | monitors + seats |
| Site24x7 | none | Web Uptime ~$10/mo ($9 annual): 25 monitors, 1-min, 32 locations | mid-tiers | Enterprise Plus $999/mo: 30s, 2500 monitors, 5-yr retention | monitors + features |
| Pingdom | none | Standard ~$50/mo ($40 annual): 50 monitors, 1-min (data fuzzy) | none | Professional ~$249/mo ($199 annual): 250+ monitors | monitors |
| Cronitor | none | $2/monitor + $5/user | none | none | pure usage |
| Healthchecks.io | yes | Business $20/mo | none | none | cron/heartbeat jobs |

Note: Pingdom figures are approximate (their page blocks automated fetches, so they
came from aggregators with 2-1 votes). UptimeRobot's 50-monitor free tier bans
commercial use post-2024.

## Verdict

Strengths:
- Frequency-based pricing is normal here, not unusual. UptimeRobot, Site24x7, Pingdom,
  and Hyperping all lead with monitor-count plus interval. Only Checkly and Cronitor are
  usage/seat-led. Our interval ladder is defensible.
- Open source / self-host is a real differentiator none of these have, and it justifies
  pricing 40-60% under the pro field.

Weaknesses:
- The old Free tier (3 monitors / 15-min / 1-day history) read as broken next to
  UptimeRobot (50 / 5-min / 3-mo) and Better Stack (10 + 10 heartbeats). This was the
  biggest gap and is now partly fixed (see decisions).
- Missing check types: every rival ships SSL-expiry, DNS, and cron/heartbeat. We have
  HTTP/TCP only. This is our clearest competitive hole.
- Retention was short across all tiers; now lengthened.

## Pricing decisions applied (pricing page, 2026-06-22)

| Plan | Price | Interval | Monitors | Retention |
|---|---|---|---|---|
| Free | $0 | 15 min | 3 -> 10 | 1 day -> 7 days |
| Hobby | "At launch" -> $7/mo | 5 min | 10 -> 25 | 7 -> 30 days |
| Professional | "At launch" -> $19/mo | 1 min | 25 -> 50 | 30 -> 90 days |
| Custom | Let's talk | bespoke | custom | 180 days |

Pricing rationale: Hobby at $7 undercuts UptimeRobot Solo ($9-10) and Site24x7 ($9-10),
far under Hyperping ($24). Professional at $19 undercuts UptimeRobot Team ($33-38),
Pingdom Standard ($40-50), Checkly Team ($64), and Hyperping Pro ($74) while matching the
1-min / multi-region / full-API feature set. The open-core story carries the value gap.

We kept the 15/5/1-min ladder rather than matching competitors' 5-min free, because the
frequency ladder is our pricing model and the generous free monitor count plus 7-day
retention removes the "broken free" signal.

## Build priorities (gaps to close)

1. SSL-expiry checks. Cheap to build, universally expected.
2. Cron / heartbeat monitoring. A whole category (Healthchecks.io, Cronitor) exists for it.
3. DNS checks. Fast follow.
4. Browser / synthetic checks. Bigger lift, can wait.

These are table stakes on every competitor and are the main reason a buyer might pick a
rival over us at the same price.

## Open questions (not answerable from competitor data)

- Cost-to-serve per monitor at 1-min and 30s across regions. The $7/$19 prices assume
  healthy margins at those frequencies; validate against real infra cost.
- How fast SSL-expiry and heartbeat can ship. If far out, Hobby/Professional may need to
  price lower to compensate.
- Whether free self-hosting feeds cloud signups or competes with the paid tiers.
- Whether Custom should publish a "from $X" anchor (like Pingdom $249, Site24x7 $999) or
  stay fully quote-based.
