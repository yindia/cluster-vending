<!-- BEGIN:nextjs-agent-rules -->

## This is NOT the Next.js you know

This version has breaking changes — APIs, conventions, and file structure may all differ from your training data. Read the relevant guide in `node_modules/next/dist/docs/` (resolved from this file's directory; in monorepos the `next` package may not be visible from the repo root) before writing any code. Heed deprecation notices.

This block is written and re-added by `next dev` — verify at `node_modules/next/dist/server/lib/generate-agent-files.js`. Removing it from a diff only re-creates the uncommitted change; committing it with your work keeps the tree clean.

<!-- END:nextjs-agent-rules -->

<!-- ASTRYX:START -->
Astryx v0.7.16 · 174 components
CLI: run every command as `npx astryx <cmd>` (shown below as `astryx ...`).

SETUP (once, in app entry e.g. main.tsx) — without these, components render unstyled:
  import "@astryxdesign/core/reset.css";
  import "@astryxdesign/core/astryx.css";

WORKFLOW — start every page from a template, never from scratch:
1. `astryx build "<idea>"` — names the template to scaffold from and the parts it lacks.
2. `astryx template <name> <path>` — scaffold it; keep its frame, gap and padding; replace the content.
3. `astryx template <Block>` for parts it lacks; `astryx component <Name>` to read props before using one.
Changing an existing page? Keep it: skip step 2 and add blocks/components inside its sections.
Before you deliver a build, run `astryx doctor --built <dir>`; don't deliver it if it fails.

RULES:
- No <div> — components handle all layout and spacing.
- Read `astryx docs layout` before changing a template's frame.
- Dense data = rows (Table, List/Item), not Card-wrapped lists. Badge = counts only.
- Style with component props first, then tokens (var(--color-*|--spacing-*|--radius-*)). No raw hex/px. No StyleX/Tailwind compiler here — don't use xstyle/utility classes directly. Some templates use StyleX and need a compiler plugin; see `astryx docs styling-overview`.
- THEMES: palettes and custom colors go through the theme, never :root overrides. To make or change a theme (colors, fonts, corners, spacing, a look from a picture), prefer the experimental theme editor over hand-written theme files: `astryx experimental theme editor new <name> --like neutral`, then `editor set <name> <thing> <value>` or `editor how "<what you want>"`; wire it with `astryx theme add <name>`; after each change look at it with `astryx experimental theme editor show <name> --png` and open the PNGs it writes (changes nothing in the app); a theme change is not done until you have looked at the pictures. In code, extend an imported theme to customize it; `theme eject <slug>` only forks source. See `astryx docs theme`.
- SELF-CHECK: re-read the file; replace any raw <div>/<span>, imported .css/@apply, or hardcoded #hex/px with the component or a token.

`astryx help` lists every command. Key ones beyond the workflow:
  search "<query>"   find any component / hook / doc / template
  discover <words>   integrations you could add
  docs <topic>       getting-started, principles, tokens, theme … (`astryx docs` lists all)
  docs cli           commands and API reference
  docs authoring     build an integration package, and every field it holds
  upgrade --from <old version> --apply   run after a dependency bump
<!-- ASTRYX:END -->

## Cluster Vending project rules

- Keep shared accounts, networks, clusters and workflows provider-neutral.
  Provider-specific fields belong behind adapters and capability checks in
  `src/lib/providers`. Only AWS has backend support today.
- Use the generated `src/gen` contracts and `src/lib/api` clients. Do not
  hand-maintain wire types. Kubernetes is the source of truth; do not add a DB.
- Cluster tiers come from the parent network; release channels belong to clusters.
- Respect etags, update masks, preview plans, plan IDs and asynchronous convergence.
  Show removals and retention/replacement details before reviewed mutations.
- User authentication and tenant permissions are enforced by the Go API. Do not
  ship shared service credentials or administrative bearer tokens to the browser.
- Main console pages use live generated Connect clients through `/api/rpc`.
  Labeled sample data is isolated to `/preview` and mocked browser tests.
- Create/edit workflows use modal steps and final review; detail pages live at
  `/accounts/[id]`, `/networks/[id]`, and `/clusters/[id]`.
- Use `DetailHeader`, `DetailSummary`, `StatusValue`, `ConsoleTable`, and
  `CopyValue` for consistent console layouts. Copy controls must accompany visible
  values, never an unlabeled second icon for an invisible ID.
- `src/app/dialogs.css` supplies native modal scroll containment;
  `console-controls.css` reveals copy actions on hover/focus and preserves touch
  access. Both are narrowly scoped browser behavior. Colors come from theme tokens.
- Helm values use `ValuesEditor`, with YAML parsing bounded before converting to
  the generated Struct contract. Keep read-only release/platform values locked.
- Background polling is off by default. `useAsyncQuery` honors tab-local Live
  updates; only explicit submitted operation tracking may bypass that preference.
- Use the existing UI server if present. Do not start duplicate listeners.
