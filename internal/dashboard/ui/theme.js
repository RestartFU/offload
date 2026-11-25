"use strict";
// Applies the saved theme before the page paints, so a light or dark choice doesn't flash the
// system's colours first. The switch itself lives in app.js.
try {
  const t = localStorage.getItem("theme");
  if (t === "light" || t === "dark") document.documentElement.dataset.theme = t;
} catch {}
