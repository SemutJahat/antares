# Web Dashboard Palettes Design

## Summary

The localhost dashboard currently has one red-on-near-black palette and a
sidebar toggle for light/dark. The red tokens go away. The dashboard gains four
named palettes — Zinc (default), Ocean, Forest, Violet — each with light and
dark variants. Palette choice lives on the Config page and is stored in the
browser. Light/dark stays on the sidebar and stays independent of palette.

## Goals

- Operators can switch Zinc, Ocean, Forest, or Violet from Config without
  saving YAML or restarting the server.
- Light and dark remain a separate control in the sidebar footer.
- Fresh visits default to Zinc + dark.
- Reload keeps both choices. An unknown stored palette falls back to Zinc.
- Login and setup screens use the same palette and mode as the rest of the app.
- Existing components that read `var(--primary)` and the other design tokens
  pick up the new palettes with no per-page color edits.

## Non-goals

- CLI / TUI theming.
- Writing the palette into `config.yaml` or syncing it across devices.
- A custom color editor, accent picker, or user-authored CSS.
- Following `prefers-color-scheme` (the sidebar toggle remains explicit).
- Redesigning layout, typography, radius, or component structure.
- Shipping the old Antares red palette as an option.

## Architecture

Two independent axes on `<html>`:

| Axis    | Mechanism                         | Storage key         | Default |
|---------|-----------------------------------|---------------------|---------|
| Mode    | class `dark` (absent = light)     | `antares.theme`     | `dark`  |
| Palette | `data-palette="zinc\|ocean\|forest\|violet"` | `antares.palette` | `zinc`  |

Example: `<html class="dark" data-palette="ocean">`.

CSS tokens stay CSS variables. `:root` is Zinc light. `.dark` without a palette
attribute is Zinc dark, so a missing attribute still looks like the default.
Each named palette overrides the same token set:

```css
[data-palette="ocean"] { /* ocean light tokens */ }
.dark[data-palette="ocean"] { /* ocean dark tokens */ }
```

A `ThemeProvider` wraps the whole `App` (not just `AppShell`) so login and
setup inherit the choice. `useTheme` in `AppShell` moves into that provider.
The provider writes `classList`, `data-palette`, `colorScheme`, and the
`theme-color` meta tag.

A tiny inline script in `web/index.html` runs before React so the first paint
matches storage. It reads the two localStorage keys, applies `dark` /
`data-palette`, and ignores malformed values.

Palette choice is client-only. It never hits `/config` and does not share the
Config page Save / restart flow.

## Palettes

One accent per palette. Surfaces take a trace of the same hue so the page reads
as one material. `--destructive`, `--success`, and `--warning` stay the same
semantic values in every palette so status colors do not drift.

Hue families (OKLCH hue), to be tuned in implementation but not renamed or
replaced:

| Palette | Surfaces | Accent | Mood |
|---------|----------|--------|------|
| Zinc    | ~260, chroma near zero | zinc gray | default, all-day work |
| Ocean   | slate ~250 | blue ~250 | cool |
| Forest  | warm gray-green ~145 | green ~150 | warm |
| Violet  | cool gray ~290 | purple ~290 | cool |

Each palette defines the existing token names only: `--background`,
`--foreground`, `--card`, `--card-foreground`, `--popover`,
`--popover-foreground`, `--primary`, `--primary-foreground`, `--secondary`,
`--secondary-foreground`, `--muted`, `--muted-foreground`, `--accent`,
`--accent-foreground`, `--destructive`, `--destructive-foreground`,
`--success`, `--warning`, `--border`, `--input`, `--ring`, `--sidebar`,
`--sidebar-foreground`.

Contrast must stay readable for body text and for primary-on-primary-foreground
buttons in both modes. No second accent color.

## Config Appearance UI

Config's section rail gains an **Appearance** item above Essentials. On small
screens it is an option in the existing section `<select>`.

Selecting Appearance shows four cards in a 2×2 grid (one column on narrow
viewports). Each card has:

- a two-stop swatch (surface + primary) for the current light/dark mode
- the palette name
- a selected ring + check when it is the active palette

Clicking a card calls `setPalette` immediately. There is no Save button and no
dirty state. Copy is utility language: heading "Appearance", one line that the
choice is stored in this browser.

Sidebar footer is unchanged: language + light/dark only.

## Files

| File | Responsibility |
|------|----------------|
| `web/src/index.css` | Replace red tokens with Zinc as `:root` / `.dark`; add the other three palettes |
| `web/index.html` | Default `data-palette="zinc"`; blocking script to apply stored mode + palette; default `theme-color` for Zinc dark |
| `web/src/lib/theme.tsx` | Palette ids, storage keys, `ThemeProvider`, `useTheme` |
| `web/src/main.tsx` or `web/src/App.tsx` | Wrap the tree with `ThemeProvider` |
| `web/src/components/layout/AppShell.tsx` | Drop local `useTheme`; consume the provider for the sidebar toggle |
| `web/src/pages/ConfigPage.tsx` | Appearance rail section + palette cards |
| `web/src/lib/i18n.tsx` and locale files | Strings for Appearance and the four palette names |

## Error handling

- Missing or garbage `antares.palette` → Zinc.
- Missing or garbage `antares.theme` → dark (same as today).
- `localStorage` throws (private mode) → in-memory Zinc + dark; picker still
  works for the session.

## Testing

This is token + picker work, not a backend change. Verification:

1. Config → Appearance: each of the four palettes restyles chrome, buttons, and
   chat immediately.
2. Sidebar light/dark still flips the same palette, not a different one.
3. Reload on `/` and on `/config` restores both palette and mode.
4. `/login` and `/setup` match the stored palette and mode.
5. With empty storage, first paint is Zinc dark (no red flash).
6. A forged `antares.palette` value falls back to Zinc.

Run the Vite dashboard (`web/`) for this. The Go binary does not need to be
running to judge tokens; gated pages can be checked once the API is up.

## Deferred

- Syncing appearance through `config.yaml` or the API.
- Per-user palettes on a shared dashboard.
- Extra palettes beyond the four named here.
