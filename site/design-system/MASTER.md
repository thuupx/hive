# Hive — design system

The source of truth for the landing page. Read this before adding a section, so
a new one matches the ones already there.

Generated with the `ui-ux-pro-max` design system for the query *"developer tool
self-hosted open source AI agent gateway technical dark mode minimal"*, then
adjusted where noted under **Deviations**.

## Pattern

**Hero + Features + CTA.** The section order the page follows:

1. Hero — the claim, the install command, one primary action
2. Problem — the pain, three cards
3. Idea — the three-layer model and the principle it comes from
4. Compare — how it differs from the tools that already exist
5. Features — what that buys you
6. Quick start — the steps, and one secondary action
7. Footer

One primary CTA per screen region. The hero CTA is the install command and the
GitHub link; every other section links out, it does not compete.

## Style

**Dark mode (OLED), dark-only.** Deep background, high contrast, eye-friendly,
power efficient. Dark-only is a deliberate choice: the palette was generated for
dark, and a second theme nobody tests is worse than one theme that is right.
Light mode is not a supported theme.

## Color

Product type: **Developer Tool / IDE** — "code dark, run green".

| Role | Value | CSS variable | Used for |
|---|---|---|---|
| Background | `#0F172A` | `--color-background` | the page |
| Foreground | `#F8FAFC` | `--color-foreground` | body text, headings |
| Card | `#1B2336` | `--color-card` | cards, surfaces above the page |
| Muted | `#272F42` | `--color-muted` | icon chips, code pills |
| Muted foreground | `#94A3B8` | `--color-muted-foreground` | secondary text |
| Border | `#475569` | `--color-border` | visible borders (terminals, buttons) |
| Hairline | `#27324A` | `--color-hairline` | section rules and dividers |
| Primary | `#1E293B` | `--color-primary` | reserved |
| Secondary | `#334155` | `--color-secondary` | reserved |
| Accent | `#22C55E` | `--color-accent` | links, emphasis, the accent phrase |
| On accent | `#0F172A` | `--color-on-accent` | text on an accent fill |
| Destructive | `#EF4444` | `--color-destructive` | the terminal's close light |
| Ring | `#22C55E` | `--color-ring` | focus outline |

Two tiers of border on purpose. A card edge reads as a seam, so it uses the
palette border at 45% opacity; a terminal is a window, so it gets the border at
full strength.

## Typography

- **Headings and body:** Inter, 400/500/600/700, loaded from Google Fonts with
  `display=swap` and preconnect.
- **Code, identifiers, the terminal:** the system monospace stack. A web font
  for code is not worth the request.
- Heading scale: `text-4xl`/`text-5xl` for the `h1`, `text-3xl`/`text-4xl` for
  section `h2`, `text-base` for card `h3`.
- Long-form text is capped at `max-w-2xl`; the hero paragraph at `max-w-xl`.

## Spacing

4/8px rhythm. Section padding is `py-20` (`sm:py-24`) everywhere, so the vertical
rhythm is one decision rather than eight. Card padding is `p-6`; grid gaps are
`gap-6`.

## Effects

Restrained. A faint 64px grid masked to the top of the hero, and one soft green
radial glow behind the headline. Both are `aria-hidden` and
`pointer-events-none`. No glow on text, no scanlines, no glitch — the generated
"cyberpunk" direction was rejected as decoration that fights readability.

Transitions are 150–300ms and limited to colour, border, opacity, and a 2px
translate on card hover.

## Accessibility rules this page holds to

- Body text contrast ≥ 4.5:1; secondary text ≥ 3:1 on the dark background.
- A visible focus ring on every interactive element, via `:focus-visible`.
- `prefers-reduced-motion: reduce` removes animation and smooth scrolling.
- A skip link to `#main`.
- Icons are inline SVG, never emoji, and decorative ones are `aria-hidden`.
- One `h1`, sections as `h2`, cards as `h3`; the comparison table uses
  `caption`, `scope="col"`, and `scope="row"`.
- The copy button announces its result in a `role="status"` region, and says so
  when the clipboard is refused rather than claiming success.

## Deviations from the generated system

1. **Focus ring is the accent, not `#1E293B`.** The generated value is almost
   the background colour. A focus ring nobody can see fails the "visible focus"
   rule the same system asks for, so it is `#22C55E`.
2. **The `Enterprise Gateway` pattern was rejected.** It recommends "Contact
   Sales" and client logos, which is wrong for an open-source, self-hosted tool.
   The page uses Hero + Features + CTA instead.
3. **Dark-only, no light theme.** See Style.
4. **Two border tiers** were added (`--color-border`, `--color-hairline`)
   because one value could not serve both a card seam and a terminal frame.

## Anti-patterns to avoid here

- Light mode as the default.
- Emoji as icons.
- Heavy neon or glitch effects, and text glow.
- More than one primary CTA in a viewport.
- Layout-shifting hover states.
- New hardcoded hex values in a component: add a token instead.
