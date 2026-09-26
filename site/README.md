# Hive landing page

The site at the root of the repository is the Go source. This directory is the
landing page, deployed to GitHub Pages by `.github/workflows/site.yml`.

```sh
cd site
npm ci          # exact versions from package-lock.json
npm run dev     # http://localhost:4321/hive/
npm run build   # -> dist/
npm run preview # serve dist/ the way Pages will
npm run og      # regenerate public/og.png from public/og.svg
```

## Why this stack

**Astro, static output, no framework runtime.** A landing page is content, and
Astro ships it as HTML. There is no client framework, so the only JavaScript on
the page is the two small scripts that need to exist (the mobile menu and the
copy button), inlined into the document. The whole page is about 8 KB of HTML
and 5.5 KB of CSS, gzipped.

The alternatives were considered:

- **A single hand-written `index.html`** with compiled Tailwind. Genuinely
  simpler, and the right answer for a page that will never grow. It was not
  chosen because components are what keep eight sections consistent, and because
  `docs/` can join the site later as Astro content collections without a rewrite.
- **Next.js.** Only worth it for an app — a playground, a dashboard. For a page
  that is entirely static it adds a server runtime and a JS bundle for no gain.

**Tailwind v4, configured in CSS.** The tokens live in `src/styles/global.css`
under `@theme`, so a colour is defined once and every utility that uses it
follows. There is no `tailwind.config.js`; v4 does not need one.

**Exact dependency versions, not ranges.** `package.json` pins `astro`,
`tailwindcss`, and `@tailwindcss/vite` without a caret, and the lockfile is
committed. A landing page has no reason to pick up a new release on its own, and
a build should not be able to resolve something different from what was reviewed.

## Structure

```text
src/
  site.ts              links and the install command, in one place
  styles/global.css    design tokens and base styles
  layouts/Base.astro   the document: meta, OG tags, fonts, skip link
  components/          one file per section, plus Icon, CommandBar, Badge
  pages/index.astro    the page, in section order
public/                favicon, OG image (and its SVG source)
design-system/         the design system this page was built from
```

`src/site.ts` exists so the repository URL and the install command cannot
disagree between the hero, the quick start, and the footer.

`Badge.astro` marks each capability as **In v1**, **Designed**, or **Planned**.
Keep that honest: anything not shipped gets a badge, and the comparison table
only compares shipped behaviour. `design-system/MASTER.md` has the rules.

## Editing prose with inline elements

Astro trims whitespace adjacent to a tag across a line break, the way JSX does.
This:

```astro
The <a href="…">design document</a>
is the source of truth.
```

renders as `Thedesign documentis the source of truth`. Write `{" "}` explicitly
around an inline element that sits mid-sentence:

```astro
The{" "}
<a href="…">design document</a>{" "}
is the source of truth.
```

This has caused two bugs on this page already. When adding prose with an inline
link, check the rendered text, not just the source.

## Design

`design-system/MASTER.md` records the palette, type, and the rules this page
follows, including the accessibility constraints (contrast, focus visibility,
reduced motion). Read it before adding a section, so a new one matches.

Two deliberate deviations from the generated design system are recorded there:
the focus ring colour, and the dark-only choice.

## Deploying

`.github/workflows/site.yml` builds and publishes on a push to `main` that
touches `site/`. It needs **Settings → Pages → Source** set to **GitHub Actions**
once.

The site is served from a project path, so `astro.config.mjs` sets `base` to
`/hive`. With a custom domain, set `base` to `/` and `site` to the domain — or
pass `SITE_BASE` and `SITE_URL` from the workflow.

## Social preview

`public/og.png` is what a link preview shows. It is a committed raster because
social platforms do not render SVG. `public/og.svg` is the editable source;
after changing it, run `npm run og` to regenerate the PNG.

The renderer (`@resvg/resvg-js`) is a devDependency only, so the site build
never depends on a native library.
