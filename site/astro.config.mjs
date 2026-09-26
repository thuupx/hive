// @ts-check
import { defineConfig } from 'astro/config';
import tailwindcss from '@tailwindcss/vite';

// A GitHub Pages project site is served from /<repo>, so the base has to match
// the repository name. With a custom domain, set base to '/' instead — or pass
// SITE_BASE and SITE_URL from the deploy workflow.
const base = process.env.SITE_BASE ?? '/hive';
const site = process.env.SITE_URL ?? 'https://thuupx.github.io';

export default defineConfig({
  site,
  base,
  vite: {
    plugins: [tailwindcss()],
  },
});
