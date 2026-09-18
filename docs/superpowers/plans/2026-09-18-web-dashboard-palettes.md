# Web Dashboard Palettes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the red dashboard tokens with four named palettes (Zinc default, Ocean, Forest, Violet) that operators pick on the Config page, while light/dark stays on the sidebar.

**Architecture:** `<html data-palette>` plus the existing `.dark` class. Pure parse/apply helpers in `web/src/lib/theme.ts`, a `ThemeProvider` wrapping `App`, CSS token overrides per palette, and a client-only Appearance section on Config. Preference lives in `localStorage`, never `config.yaml`.

**Tech Stack:** React 19, Vite, Tailwind v4 CSS variables, Bun tests, existing `useLocalStorage`.

**Spec:** `docs/superpowers/specs/2026-09-18-web-dashboard-palettes-design.md`

---

### Task 1: Palette parse/apply helpers (TDD)

**Files:**
- Create: `web/src/lib/theme.ts`
- Create: `web/src/lib/theme.test.mjs`

- [ ] **Step 1: Write the failing test**

```js
import { describe, expect, test } from 'bun:test'
import {
  DEFAULT_MODE,
  DEFAULT_PALETTE,
  PALETTES,
  PALETTE_THEME_COLORS,
  applyAppearance,
  parseColorMode,
  parsePalette,
  readStoredColorMode,
  readStoredPalette,
} from './theme.ts'

describe('parsePalette', () => {
  test('accepts the four named palettes and falls back to zinc', () => {
    expect(PALETTES).toEqual(['zinc', 'ocean', 'forest', 'violet'])
    expect(parsePalette('ocean')).toBe('ocean')
    expect(parsePalette('red')).toBe(DEFAULT_PALETTE)
    expect(parsePalette(undefined)).toBe('zinc')
  })
})

describe('parseColorMode', () => {
  test('accepts light and dark and falls back to dark', () => {
    expect(parseColorMode('light')).toBe('light')
    expect(parseColorMode('dark')).toBe('dark')
    expect(parseColorMode('system')).toBe(DEFAULT_MODE)
  })
})

describe('readStored*', () => {
  test('reads JSON localStorage values and ignores garbage', () => {
    const store = {
      'antares.palette': '"forest"',
      'antares.theme': '"light"',
    }
    const getItem = (k) => store[k] ?? null
    expect(readStoredPalette(getItem)).toBe('forest')
    expect(readStoredColorMode(getItem)).toBe('light')
  })

  test('falls back when JSON is invalid or the palette is unknown', () => {
    expect(readStoredPalette((k) => (k === 'antares.palette' ? 'nope' : null))).toBe('zinc')
    expect(readStoredPalette((k) => (k === 'antares.palette' ? '"red"' : null))).toBe('zinc')
    expect(readStoredColorMode(() => '{')).toBe('dark')
    expect(readStoredColorMode(() => { throw new Error('blocked') })).toBe('dark')
  })
})

describe('applyAppearance', () => {
  test('sets dark class, data-palette, color-scheme, and theme-color', () => {
    const toggled = []
    const attrs = {}
    const style = { colorScheme: '' }
    const meta = { content: '#000' }
    applyAppearance(
      {
        classList: { toggle: (token, force) => toggled.push([token, force]) },
        setAttribute: (name, value) => { attrs[name] = value },
        style,
      },
      { setAttribute: (name, value) => { if (name === 'content') meta.content = value } },
      'light',
      'ocean',
    )
    expect(toggled).toEqual([['dark', false]])
    expect(attrs['data-palette']).toBe('ocean')
    expect(style.colorScheme).toBe('light')
    expect(meta.content).toBe(PALETTE_THEME_COLORS.ocean.light)
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && bun test src/lib/theme.test.mjs`

Expected: FAIL — `theme.ts` is not found.

- [ ] **Step 3: Write minimal implementation** in `web/src/lib/theme.ts` (exports listed in the test).

- [ ] **Step 4: Re-run the test and confirm it passes.**

- [ ] **Step 5: Commit** only if the user asked.

---

### Task 2: CSS palettes + first-paint script

**Files:**
- Modify: `web/src/index.css`
- Modify: `web/index.html`

- [ ] **Step 1:** Replace the red `:root` / `.dark` tokens with Zinc. Keep `--destructive` / `--success` / `--warning` only on `:root` and `.dark`. Add `[data-palette="ocean"|"forest"|"violet"]` and matching `.dark[data-palette="…"]` overrides for surfaces + primary + ring. Keep `@theme inline` mapped to the same variable names.

- [ ] **Step 2:** In `web/index.html`, set `data-palette="zinc"` on `<html>`, change `theme-color` to Zinc dark `#18181b`, and add a blocking inline script that JSON-parses `antares.theme` and `antares.palette` the same way as `readStored*` and calls the same defaults on garbage.

- [ ] **Step 3:** Visual check later with Vite. No backend required for token paint.

---

### Task 3: ThemeProvider at the App root

**Files:**
- Create: `web/src/lib/theme.tsx`
- Modify: `web/src/App.tsx`
- Modify: `web/src/components/layout/AppShell.tsx`

- [ ] **Step 1:** `ThemeProvider` uses `useLocalStorage` with `PALETTE_KEY` / `THEME_KEY`, parses on read, and `useEffect`s `applyAppearance(document.documentElement, meta, mode, palette)`. Export `useTheme()` returning `{ theme, setTheme, palette, setPalette }`.

- [ ] **Step 2:** Wrap `<I18nProvider>` with `<ThemeProvider>` in `App.tsx`.

- [ ] **Step 3:** Delete local `useTheme` in `AppShell.tsx`; import from `@/lib/theme`. Sidebar light/dark toggle stays.

---

### Task 4: Config Appearance picker + i18n

**Files:**
- Modify: `web/src/pages/ConfigPage.tsx`
- Modify: `web/src/lib/i18n.tsx`
- Modify: `web/src/lib/locales/id.ts`, `ja.ts`, `zh.ts`, `ru.ts`

- [ ] **Step 1:** Add keys next to the existing `theme.*` / `config.*` keys:

```
'theme.zinc': 'Zinc'
'theme.ocean': 'Ocean'
'theme.forest': 'Forest'
'theme.violet': 'Violet'
'config.appearance': 'Appearance'
'config.appearanceHint': 'Stored in this browser. Light and dark stay on the sidebar.'
```

Locales: id Appearance/Tampilan + palet names unchanged; ja 外観; zh 外观; ru Внешний вид.

- [ ] **Step 2:** `APPEARANCE = '__appearance'` rail item above Essentials. Section renders 2×2 palette cards (1 col on narrow). Each card uses `PALETTE_SWATCHES[palette][theme]` so previews stay distinct. Click calls `setPalette` immediately. No Save / dirty state.

---

### Task 5: Verify

- [ ] `cd web && bun test src/lib/theme.test.mjs`
- [ ] `cd web && bun run typecheck`
- [ ] Vite: first paint Zinc dark; picker applies palettes; light/dark independent; reload persists. Login/setup inherit provider. Forged `antares.palette` falls back to Zinc.

---

## Spec coverage

| Spec requirement | Task |
|---|---|
| Four palettes, drop red, default Zinc dark | 1, 2 |
| `data-palette` + `.dark` | 2, 3 |
| localStorage, not YAML | 1, 3, 4 |
| ThemeProvider at App root | 3 |
| FOUC script | 2 |
| Config Appearance cards | 4 |
| Sidebar light/dark unchanged | 3 |
| Garbage fallback | 1, 2 |
| Semantic status colors shared | 2 |
