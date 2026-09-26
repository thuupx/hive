// Renders public/og.svg to public/og.png.
//
// Social platforms do not render SVG for a link preview, so the OG image has to
// be a raster. The SVG stays the editable source and the PNG is committed, which
// keeps the site build free of a native dependency.
//
//   npm run og
import { readFileSync, writeFileSync } from "node:fs";
import { Resvg } from "@resvg/resvg-js";

const source = new URL("../public/og.svg", import.meta.url);
const target = new URL("../public/og.png", import.meta.url);

const resvg = new Resvg(readFileSync(source, "utf8"), {
  fitTo: { mode: "width", value: 1200 },
  // The SVG names Inter first. It falls back to whatever the machine has, which
  // is fine for a wordmark and keeps the script from needing a font file.
  font: { loadSystemFonts: true, defaultFontFamily: "Helvetica" },
});

writeFileSync(target, resvg.render().asPng());
console.log("wrote public/og.png (1200x630)");
