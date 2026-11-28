"use strict";
// offload dashboard. Plain DOM, no build step: everything user- or runner-supplied goes in as
// text nodes (never innerHTML), and the page talks only to the local dashboard server.

// ---------- tiny DOM helpers ----------
const SVGNS = "http://www.w3.org/2000/svg";
function build(el, attrs, kids) {
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === "style") Object.assign(el.style, v); // CSSOM: allowed under the page's CSP
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : String(v));
  }
  for (const c of kids.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}
const h = (tag, attrs, ...kids) => build(document.createElement(tag), attrs, kids);
const s = (tag, attrs, ...kids) => build(document.createElementNS(SVGNS, tag), attrs, kids);
const $ = (sel) => document.querySelector(sel);

// ---------- formatting ----------
const GiB = 1024 ** 3, MiB = 1024 ** 2;
function fmtBytes(b) {
  if (b == null || isNaN(b)) return "–";
  if (b === 0) return "0";
  if (Math.abs(b) >= 1024 * GiB) return (b / (1024 * GiB)).toFixed(1) + " TiB";
  if (Math.abs(b) >= GiB) return (b / GiB).toFixed(b >= 10 * GiB ? 0 : 1) + " GiB";
  if (Math.abs(b) >= MiB) return Math.round(b / MiB) + " MiB";
  return Math.round(b / 1024) + " KiB";
}
function fmtCores(c) {
  if (c == null || isNaN(c)) return "–";
  if (Math.abs(c) >= 100) return String(Math.round(c));
  return (Math.round(c * 10) / 10).toFixed(1).replace(/\.0$/, "");
}
function fmtDur(sec) {
  if (sec == null || isNaN(sec)) return "–";
  sec = Math.max(0, sec);
  if (sec < 1) return (sec * 1000).toFixed(0) + "ms";
  if (sec < 60) return sec.toFixed(sec < 10 ? 1 : 0).replace(/\.0$/, "") + "s";
  const m = Math.floor(sec / 60), s2 = Math.floor(sec % 60);
  if (sec < 3600) return m + "m " + String(s2).padStart(2, "0") + "s";
  const hr = Math.floor(sec / 3600), m2 = Math.floor((sec % 3600) / 60);
  if (sec < 86400) return hr + "h " + String(m2).padStart(2, "0") + "m";
  return Math.floor(sec / 86400) + "d " + Math.floor((sec % 86400) / 3600) + "h";
}
const since = (t) => (Date.now() - new Date(t).getTime()) / 1000;
const ago = (t) => fmtDur(since(t)) + " ago";
const pct = (x) => (x == null || isNaN(x) ? "–" : Math.round(x * 100) + "%");
function fmtTime(t) {
  const d = new Date(t);
  const today = new Date().toDateString() === d.toDateString();
  return today ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })
    : d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}
function quantile(xs, q) {
  if (!xs.length) return null;
  const a = [...xs].sort((x, y) => x - y);
  const i = (a.length - 1) * q, lo = Math.floor(i), hi = Math.ceil(i);
  return a[lo] + (a[hi] - a[lo]) * (i - lo);
}
const sum = (xs, f = (x) => x) => xs.reduce((a, x) => a + (f(x) || 0), 0);
const shortKey = (k) => (k ? k.replace(/--b[0-9a-f]+$/, "") : "");
function cmdline(argv) {
  return (argv || []).map((a) => (/^[\w@%+=:,./-]+$/.test(a) ? a : "'" + a.replace(/'/g, "'\\''") + "'")).join(" ");
}
const jobCmd = (j) => (j.github ? (j.github.job || "Waiting for a GitHub job") : cmdline(j.spec.argv));
function stripAnsi(t) { return t.replace(/\x1b\[[0-9;?]*[ -\/]*[@-~]/g, "").replace(/\x1b\][^\x07]*\x07/g, ""); }

// ---------- icons (status always pairs an icon with a label) ----------
function icon(kind) {
  const p = {
    good: [s("circle", { cx: 8, cy: 8, r: 7, fill: "currentColor" }), s("path", { d: "M4.8 8.2l2.2 2.2 4.2-4.6", fill: "none", stroke: "#fff", "stroke-width": 1.8, "stroke-linecap": "round", "stroke-linejoin": "round" })],
    warning: [s("path", { d: "M8 1.2l7 12.6H1z", fill: "currentColor", "stroke-linejoin": "round" }), s("path", { d: "M8 6v3.6", stroke: "#0b0b0b", "stroke-width": 1.7, "stroke-linecap": "round" }), s("circle", { cx: 8, cy: 11.7, r: 0.95, fill: "#0b0b0b" })],
    serious: [s("rect", { x: 1, y: 1, width: 14, height: 14, rx: 3, fill: "currentColor" }), s("path", { d: "M8 4.2v4.6", stroke: "#fff", "stroke-width": 1.8, "stroke-linecap": "round" }), s("circle", { cx: 8, cy: 11.4, r: 1, fill: "#fff" })],
    critical: [s("circle", { cx: 8, cy: 8, r: 7, fill: "currentColor" }), s("path", { d: "M5.4 5.4l5.2 5.2M10.6 5.4l-5.2 5.2", stroke: "#fff", "stroke-width": 1.8, "stroke-linecap": "round" })],
    neutral: [s("circle", { cx: 8, cy: 8, r: 6, fill: "none", stroke: "currentColor", "stroke-width": 2 })],
    info: [s("circle", { cx: 8, cy: 8, r: 7, fill: "currentColor" }), s("path", { d: "M8 7.2v4.2", stroke: "#fff", "stroke-width": 1.8, "stroke-linecap": "round" }), s("circle", { cx: 8, cy: 4.7, r: 1, fill: "#fff" })],
    running: [s("circle", { cx: 8, cy: 8, r: 7, fill: "currentColor" }), s("path", { d: "M6.4 5v6l4.6-3z", fill: "#fff" })],
    paused: [s("circle", { cx: 8, cy: 8, r: 7, fill: "currentColor" }), s("path", { d: "M6.2 5.2v5.6M9.8 5.2v5.6", stroke: "#fff", "stroke-width": 1.8, "stroke-linecap": "round" })],
  }[kind] || [];
  return s("svg", { viewBox: "0 0 16 16", "aria-hidden": "true" }, p);
}
function chip(kind, label) {
  const cls = kind === "running" ? "info" : kind === "paused" ? "neutral" : kind;
  return h("span", { class: "chip " + cls }, icon(kind), label);
}
function stateChip(state, reason) {
  switch (state) {
    case "running": return chip("running", "Running");
    case "queued": return chip("warning", reason && reason.includes("building") ? "Building env" : "Queued");
    case "uploading": return chip("neutral", "Uploading");
    case "exited": return chip(reason ? "serious" : "good", "Exited");
    case "failed": return chip("critical", reason && reason.startsWith("out of memory") ? "Out of memory" : "Failed");
    case "cancelled": return chip("neutral", "Cancelled");
    default: return chip("neutral", state || "?");
  }
}
function exitChip(w) {
  if (stale(w)) {
    const c = chip("neutral", "Unknown");
    c.title = `${w.runner} isn't answering. This is what it last reported` + (w.host.last_seen ? `, ${ago(w.host.last_seen)}` : "") + ".";
    return c;
  }
  if (w.state === "exited" && w.exit_code != null && w.exit_code !== 0) return chip("serious", "Exit " + w.exit_code);
  return stateChip(w.state, w.reason);
}

// ---------- tooltip ----------
const tip = $("#tooltip");
function showTip(x, y, content) {
  tip.replaceChildren(...[content].flat());
  tip.hidden = false;
  const r = tip.getBoundingClientRect();
  let left = x + 14, top = y + 14;
  if (left + r.width > innerWidth - 8) left = x - r.width - 14;
  if (top + r.height > innerHeight - 8) top = y - r.height - 14;
  tip.style.left = Math.max(8, left) + "px";
  tip.style.top = Math.max(8, top) + "px";
}
const hideTip = () => { tip.hidden = true; };
function tipFor(el, fn) {
  el.addEventListener("pointermove", (e) => showTip(e.clientX, e.clientY, fn()));
  el.addEventListener("pointerleave", hideTip);
  el.addEventListener("focus", () => { const r = el.getBoundingClientRect(); showTip(r.right, r.bottom, fn()); });
  el.addEventListener("blur", hideTip);
}

// ---------- state ----------
const store = (k, v) => { try { if (v === undefined) return localStorage.getItem(k); localStorage.setItem(k, v); } catch { return null; } };

// The per-run key arrives in the link's fragment (#key=…), which the browser never sends to the
// server. It is kept in this origin's storage (scoped to the port, unlike a cookie) so other tabs
// and reloads work, and sent with every API request.
let KEY = store("key") || "";
// takeKey picks the key out of the address, on load or when a new link is pasted into an open tab.
function takeKey() {
  const m = location.hash.match(/^#key=([0-9a-f]+)$/);
  if (!m) return;
  KEY = m[1];
  store("key", KEY);
  history.replaceState(null, "", location.pathname + "#/overview"); // out of the address bar and history
}
takeKey();
const withKey = (opts = {}) => ({ ...opts, headers: { ...(opts.headers || {}), "X-Offload-Key": KEY } });
const state = {
  fleet: null, fleetErr: null, fleetAt: 0,
  view: null,
  range: store("range") || "1h",
  runner: store("runner") || "",
  series: {}, seriesFor: "", seriesLoading: false,
  ledger: null, ledgerFor: "", ledgerErrors: null, ledgerTruncated: [],
  cfilter: { state: store("cstate") || "active", kind: "", runner: "", q: "" },
  gfilter: { state: store("gstate") || "active", owner: "" },
  ghFormOpen: false, ghFormBusy: false, ghFormMsg: null, ghNotice: null, ghArmed: "", ghBusyKey: "",
  copied: "", // the snippet a copy button just copied
  sel: null, // {runner, id}
  stopping: new Map(), // "runner/id" -> "" while stopping, or the error
  pausing: new Map(), // runner name -> "" while pausing or resuming, or the error
  usage: new Map(), // "runner/id" -> [{t, cpu, mem}]
};
const RANGES = [["1h", 3600], ["6h", 6 * 3600], ["24h", 86400], ["7d", 7 * 86400]];
const rangeSecs = () => (RANGES.find((r) => r[0] === state.range) || RANGES[0])[1];

const NOKEY = "No key for this dashboard: open the link offload dashboard printed";
async function api(path, opts) {
  const r = await fetch(path, withKey(opts));
  if (r.status === 401) throw new Error(NOKEY);
  if (!r.ok) {
    let msg = r.statusText;
    try { msg = (await r.json()).error || msg; } catch {}
    throw new Error(msg);
  }
  return r.json();
}

// ---------- data loops ----------
async function pollFleet() {
  try {
    const first = !state.fleet;
    state.fleet = await api("/api/fleet");
    for (const r of state.fleet.runners) if (r.status) r.status.allocated = { cpu: 0, mem: 0, ...r.status.allocated }; // zeros are omitted
    state.fleetErr = null;
    if (first && state.view === "overview") scheduleData(); // series need the runner list
    state.fleetAt = Date.now();
    recordUsage();
  } catch (e) {
    state.fleetErr = e.message;
  }
  try { render(); } finally { setTimeout(pollFleet, 2000); } // a render bug mustn't stop the updates
}

function recordUsage() {
  const now = Date.now();
  const listed = new Set();
  for (const r of state.fleet.runners) for (const w of [...r.jobs, ...r.sessions]) listed.add(r.name + "/" + w.id);
  for (const k of state.usage.keys()) if (!listed.has(k)) state.usage.delete(k);
  for (const r of state.fleet.runners) {
    for (const w of r.metrics?.workloads || []) {
      const k = r.name + "/" + w.id;
      const arr = state.usage.get(k) || [];
      if (!arr.length || arr[arr.length - 1].at !== r.metrics.at) arr.push({ t: now, at: r.metrics.at, cpu: w.cpu, mem: w.mem });
      if (arr.length > 150) arr.shift();
      state.usage.set(k, arr);
    }
  }
}

const metricRunners = () => (state.fleet?.runners || []).filter((r) => r.current && (!state.runner || r.name === state.runner));

// gen is the schedule that asked (see scheduleData): an answer for an older one is dropped.
async function loadSeries(gen) {
  const key = state.range + "|" + state.runner;
  const runners = metricRunners();
  if (!runners.length) { state.series = {}; state.seriesFor = key; state.seriesLoading = false; renderCharts(); return; }
  state.seriesLoading = true;
  renderCharts();
  const out = {};
  await Promise.all(runners.map(async (r) => {
    try { out[r.name] = await api(`/api/series?host=${encodeURIComponent(r.name)}&range=${state.range}`); } catch {}
  }));
  if (gen !== dataGen || key !== state.range + "|" + state.runner) return; // the filter changed meanwhile
  state.series = out; state.seriesFor = key; state.seriesLoading = false;
  renderCharts();
}

async function loadLedger(gen) {
  const key = state.range;
  const since = new Date(Date.now() - rangeSecs() * 1000).toISOString().replace(/\.\d+Z$/, "Z");
  try {
    const res = await api("/api/ledger?since=" + encodeURIComponent(since));
    if (gen !== dataGen || key !== state.range) return;
    state.ledger = res.rows; state.ledgerErrors = res.errors || null; state.ledgerFor = key;
    state.ledgerTruncated = res.truncated || [];
  } catch (e) {
    if (gen !== dataGen) return;
    state.ledgerErrors = { dashboard: e.message };
  }
  renderActivity();
}

let seriesTimer, ledgerTimer, dataGen = 0;
function scheduleData() {
  clearTimeout(seriesTimer); clearTimeout(ledgerTimer);
  // A load still in flight from an earlier schedule must not queue another loop when it ends.
  const gen = ++dataGen;
  const tickSeries = async () => { if (state.view === "overview") await loadSeries(gen); if (gen === dataGen) seriesTimer = setTimeout(tickSeries, state.range === "1h" ? 5000 : 30000); };
  const tickLedger = async () => { if (state.view === "overview") await loadLedger(gen); if (gen === dataGen) ledgerTimer = setTimeout(tickLedger, 30000); };
  tickSeries(); tickLedger();
}

// ---------- routing ----------
function route() {
  takeKey();
  const parts = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  const view = ["containers", "github", "devices"].includes(parts[0]) ? parts[0] : "overview";
  const changed = view !== state.view;
  state.view = view;
  const sel = view !== "overview" && parts[1] && parts[2] ? { runner: parts[1], id: parts[2] } : null;
  if (JSON.stringify(sel) !== JSON.stringify(state.sel)) { state.sel = sel; openDrawer(); }
  for (const a of document.querySelectorAll(".tabs a")) {
    if (a.dataset.view === view) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  }
  if (changed) { mainBuilt = ""; if (view === "overview") scheduleData(); if (view === "devices") loadDevices(); }
  render();
}
addEventListener("hashchange", route);

// ---------- aggregates ----------
function totals(runners) {
  const on = runners.filter((r) => r.online && r.status);
  const t = {
    runners: runners.length, online: on.length,
    capCPU: sum(on, (r) => r.status.capacity.cpu), allocCPU: sum(on, (r) => r.status.allocated.cpu),
    hostCPUs: sum(on, (r) => r.status.host_cpus || r.metrics?.cpus?.length),
    busy: on.some((r) => r.metrics) ? sum(on, (r) => sum(r.metrics?.cpus || [], (c) => c.busy)) : null,
    capMem: sum(on, (r) => r.status.capacity.mem), allocMem: sum(on, (r) => r.status.allocated.mem),
    memAvail: sum(on, (r) => r.status.mem_available), memTotal: sum(on, (r) => r.metrics?.mem_total),
    running: sum(on, (r) => r.status.running), queued: sum(on, (r) => r.status.queued), sessions: sum(on, (r) => r.status.sessions),
  };
  t.freeCPU = Math.max(0, t.capCPU - t.allocCPU);
  t.freeMem = Math.max(0, t.capMem - t.allocMem);
  return t;
}
const scoped = () => (state.fleet?.runners || []).filter((r) => !state.runner || r.name === state.runner);

function alertsFor(runners, version) {
  const out = [];
  for (const r of runners) {
    if (!r.online) { out.push(["critical", [h("b", {}, r.name), " is not answering", r.last_seen ? " (last seen " + ago(r.last_seen) + ")" : "", r.error ? ": " + r.error : ""]]); continue; }
    const st = r.status;
    if (!st) continue;
    if (r.error) out.push(["warning", [h("b", {}, r.name), ": ", r.error]]);
    const perr = state.pausing.get(r.name);
    if (perr) out.push(["critical", [h("b", {}, r.name), ": ", perr]]);
    if (st.paused) out.push(["info", [h("b", {}, r.name), " is paused: it refuses new jobs and sessions until you resume it"]]);
    else if (!st.accepting) out.push(["warning", [h("b", {}, r.name), " is not accepting work: ", st.reason || "unknown reason"]]);
    if (st.mem_available < st.mem_reserve * 1.25) out.push(["serious", [h("b", {}, r.name), ": host memory is close to the reserve (", fmtBytes(st.mem_available), " available, reserve ", fmtBytes(st.mem_reserve), ")"]]);
    for (const d of r.metrics?.disk || []) {
      const free = d.total ? d.free / d.total : 1;
      if (free < 0.1) out.push(["critical", [h("b", {}, r.name), `: the ${d.name} disk is ${pct(1 - free)} full (${fmtBytes(d.free)} free)`]]);
      else if (free < 0.2) out.push(["warning", [h("b", {}, r.name), `: the ${d.name} disk is ${pct(1 - free)} full (${fmtBytes(d.free)} free)`]]);
    }
    if (version && st.version && st.version !== version && version !== "dev") out.push(["info", [h("b", {}, r.name), ` runs ${st.version}; this machine has ${version}. Upgrade with `, h("code", {}, "offload host upgrade " + r.name)]]);
    else if (!st.metrics) out.push(["info", [h("b", {}, r.name), " predates usage metrics, so its cores, charts and statistics are missing. Upgrade with ", h("code", {}, "offload host upgrade " + r.name)]]);
  }
  return out;
}

// ---------- render ----------
let mainBuilt = "";
function render() {
  renderLive();
  if (!state.fleet && !state.fleetErr) return;
  if (state.view === "overview") renderOverview(); else if (state.view === "github") renderGitHubView(); else if (state.view === "devices") { if (mainBuilt !== "devices") renderDevices(); } else renderContainers();
  renderTabCounts();
  if (state.sel) renderDrawer();
}

function renderLive() {
  const el = $("#live");
  const stale = state.fleetErr || (state.fleetAt && Date.now() - state.fleetAt > 8000);
  el.classList.toggle("stale", !!stale);
  const txt = state.fleetErr === NOKEY ? NOKEY : state.fleetErr ? "Dashboard not responding" : state.fleetAt ? "Live" : "Connecting…";
  const on = state.fleet ? state.fleet.runners.filter((r) => r.online).length : 0;
  const all = state.fleet ? state.fleet.runners.length : 0;
  el.replaceChildren(h("span", { class: "dot", "aria-hidden": "true" }), h("span", { class: "txt" }, txt + (state.fleet ? ` · ${on} of ${all} runner${all === 1 ? "" : "s"} online` : "")));
}

function segmented(options, value, onChange, label) {
  return h("div", { class: "seg", role: "group", "aria-label": label },
    options.map(([v, text]) => h("button", { type: "button", "aria-pressed": String(v === value), onclick: () => onChange(v) }, text)));
}
function runnerSelect(value, onChange) {
  const names = (state.fleet?.runners || []).map((r) => r.name);
  return h("select", { "aria-label": "Runner", onchange: (e) => onChange(e.target.value) },
    h("option", { value: "" }, "All runners"),
    names.map((n) => { const o = h("option", { value: n }, n); if (n === value) o.selected = true; return o; }));
}

// Overview: built once per visit, then each section re-renders in place.
function renderOverview() {
  const main = $("#main");
  if (mainBuilt !== "overview") {
    mainBuilt = "overview";
    main.replaceChildren(
      h("div", { class: "filters", id: "ov-filters" }),
      h("div", { class: "alerts", id: "ov-alerts" }),
      h("section", {}, h("div", { class: "kpis", id: "ov-kpis" })),
      h("section", {}, h("div", { class: "section-head" }, h("h2", {}, "Usage over time"), h("span", { class: "muted", id: "ov-charts-sub" })), h("div", { class: "grid two", id: "ov-charts" })),
      h("section", { id: "ov-runners-sec" }, h("div", { class: "section-head" }, h("h2", {}, "Runners"), h("span", { class: "muted" }, "Each square is a CPU, shaded by how busy it is")), h("div", { class: "grid runners", id: "ov-runners" })),
      h("section", { id: "ov-queue" }),
      h("section", { id: "ov-activity" }),
    );
    renderFilters(); renderCharts(); renderActivity();
  }
  if (!state.fleet) {
    $("#ov-kpis").replaceChildren(h("div", { class: "card empty-state" }, state.fleetErr));
    return;
  }
  if (!state.fleet.runners.length) {
    $("#ov-kpis").replaceChildren(h("div", { class: "card empty-state", style: { gridColumn: "1 / -1" } },
      h("p", {}, "No runners found yet. The dashboard keeps looking every 30 seconds."),
      h("p", {}, "Install one on a tailnet machine with ", h("code", {}, "offload runner install"), ".")));
    return;
  }
  if (filtersKey !== runnerNames()) renderFilters();
  const runners = scoped();
  $("#ov-alerts").replaceChildren(...alertsFor(runners, state.fleet.version).map(([k, msg]) => h("div", { class: "alert " + k, role: k === "critical" ? "alert" : null }, icon(k), h("div", {}, msg))));
  renderKPIs(runners);
  const focused = document.activeElement?.dataset?.pause;
  $("#ov-runners").replaceChildren(...runners.map(runnerCard));
  if (focused != null) [...document.querySelectorAll("#ov-runners [data-pause]")].find((b) => b.dataset.pause === focused)?.focus({ preventScroll: true });
  renderQueue(runners);
}

let filtersKey = "";
const runnerNames = () => (state.fleet?.runners || []).map((r) => r.name).join("\n");
function renderFilters() {
  const el = $("#ov-filters");
  if (!el) return;
  filtersKey = runnerNames();
  el.replaceChildren(
    segmented(RANGES.map(([r]) => [r, r === "1h" ? "Last hour" : r === "6h" ? "6 hours" : r === "24h" ? "24 hours" : "7 days"]), state.range, (v) => {
      state.range = v; store("range", v); renderFilters(); scheduleData();
    }, "Time range"),
    runnerSelect(state.runner, (v) => { state.runner = v; store("runner", v); render(); scheduleData(); }),
  );
}

function kpi(label, value, sub, extra, hero) {
  return h("div", { class: "card kpi" + (hero ? " hero" : "") }, h("div", { class: "label" }, label), h("div", { class: "value" }, value), sub && h("div", { class: "sub" }, sub), extra);
}
function meter(frac, cls, marker) {
  const f = Math.max(0, Math.min(1, frac || 0));
  return h("div", { class: "meter", role: "img", "aria-label": pct(f) },
    h("span", { class: cls || "", style: { width: f * 100 + "%" } }),
    marker != null && h("i", { style: { left: `calc(${Math.max(0, Math.min(1, marker)) * 100}% - 1px)` } }));
}

function renderKPIs(runners) {
  const t = totals(runners);
  $("#ov-kpis").replaceChildren(
    kpi("Free cores", h("span", {}, fmtCores(t.freeCPU), " ", h("small", {}, "of " + fmtCores(t.capCPU))),
      `Guaranteed CPU not yet handed to jobs or sessions (${fmtCores(t.allocCPU)} is)`, null, true),
    kpi("Busy right now", t.busy == null ? "–" : h("span", {}, fmtCores(t.busy), " ", h("small", {}, "cores")),
      t.busy == null ? "Runners need an upgrade for live usage" : `${pct(t.hostCPUs ? t.busy / t.hostCPUs : 0)} of ${t.hostCPUs} CPUs, whole hosts`),
    kpi("Memory free", fmtBytes(t.freeMem), `of ${fmtBytes(t.capMem)} not yet booked · hosts have ${fmtBytes(t.memAvail)} available`),
    kpi("Workloads", h("span", {}, String(t.running), " ", h("small", {}, "running")), `${t.queued} queued · ${t.sessions} session${t.sessions === 1 ? "" : "s"}`),
    kpi("Runners", h("span", {}, String(t.online), " ", h("small", {}, "of " + t.runners)), t.online === t.runners ? "All answering" : `${t.runners - t.online} not answering`),
  );
}

function heatClass(busy) {
  if (busy < 0.05) return "";
  return "h" + Math.min(6, Math.max(1, Math.ceil(busy * 6)));
}

function workloadLabel(r, id) {
  const j = r.jobs.find((x) => x.id === id);
  if (j) return id + " · " + truncate(jobCmd(j), 60);
  const se = r.sessions.find((x) => x.id === id);
  return se ? id + " · session " + se.profile : id;
}
const truncate = (t, n) => (t.length > n ? t.slice(0, n - 1) + "…" : t);

function runnerCard(r) {
  const st = r.status;
  const head = h("div", { class: "runner-head" }, h("h3", {}, r.name),
    !r.online ? chip("critical", "Offline") : st?.paused ? chip("paused", "Paused") : st && !st.accepting ? chip("warning", "Not accepting") : chip("good", "Accepting"),
    r.online && st ? pauseBtn(r) : null);
  if (!st) return h("div", { class: "card" }, head, h("p", { class: "muted" }, r.error || "No status yet"));
  const meta = [st.hostname, st.os + "/" + st.arch, st.version, st.runtime === "container" ? "containers" : "host runtime"];
  if (st.started && st.metrics) meta.push("up " + fmtDur(since(st.started)));
  const m = r.metrics;
  const freeCPU = Math.max(0, st.capacity.cpu - st.allocated.cpu);
  let cores;
  if (m && m.cpus.length) {
    const busy = sum(m.cpus, (c) => c.busy);
    cores = [
      h("div", { class: "cores-head" }, h("span", { class: "k" }, "Cores"),
        h("span", { class: "num" }, `${fmtCores(freeCPU)} of ${fmtCores(st.capacity.cpu)} free · ${fmtCores(busy)} of ${m.cpus.length} busy`)),
      h("div", { class: "cores", role: "list", "aria-label": `CPUs of ${r.name}` }, m.cpus.map((c) => {
        const cell = h("div", { class: ["cell", heatClass(c.busy), c.reserved ? "reserved" : "", c.owners?.length ? "pinned" : ""].join(" "), role: "listitem", tabindex: 0,
          "aria-label": `CPU ${c.id}, ${pct(c.busy)} busy${c.reserved ? ", reserved for the system" : ""}${c.owners?.length ? ", pinned to " + c.owners.join(", ") : ""}` });
        tipFor(cell, () => [
          h("div", { class: "t" }, "CPU " + c.id + (c.reserved ? " · reserved for the system" : "")),
          h("div", { class: "r" }, h("b", {}, pct(c.busy)), h("span", {}, "busy")),
          ...(c.owners || []).map((id) => h("div", { class: "r" }, h("span", {}, "pinned: " + workloadLabel(r, id)))),
        ]);
        cell.addEventListener("click", () => { if (c.owners?.length) location.hash = workloadHash(r, c.owners[0]); });
        return cell;
      })),
      h("div", { class: "legend-cores" },
        h("span", {}, "Idle", h("span", { class: "ramp", "aria-hidden": "true" }, ["idle", "heat-1", "heat-2", "heat-3", "heat-4", "heat-5", "heat-6"].map((v) => h("i", { style: { background: `var(--${v})` } }))), "Busy"),
        h("span", {}, h("span", { class: "sw cell pinned", style: { display: "inline-block" }, "aria-hidden": "true" }), "Pinned to a job"),
        m.cpus.some((c) => c.reserved) && h("span", {}, h("span", { class: "sw cell reserved", "aria-hidden": "true" }), "Kept for the system")),
    ];
  } else {
    cores = [h("div", { class: "cores-head" }, h("span", { class: "k" }, "Cores"), h("span", { class: "num" }, `${fmtCores(freeCPU)} of ${fmtCores(st.capacity.cpu)} free`)),
      meter(st.capacity.cpu ? st.allocated.cpu / st.capacity.cpu : 0)];
  }
  const memFrac = st.capacity.mem ? st.allocated.mem / st.capacity.mem : 0;
  const rows = [
    h("div", { class: "meter-row" }, h("span", { class: "k" }, "CPU"), meter(st.capacity.cpu ? st.allocated.cpu / st.capacity.cpu : 0),
      h("span", { class: "v num" }, `${fmtCores(st.allocated.cpu)} of ${fmtCores(st.capacity.cpu)} cores guaranteed to work · load ${st.load1.toFixed(1)}`)),
    h("div", { class: "meter-row" }, h("span", { class: "k" }, "Memory"), meter(memFrac, memFrac > 0.9 ? "warn" : ""),
      h("span", { class: "v num" }, `${fmtBytes(st.allocated.mem)} of ${fmtBytes(st.capacity.mem)} booked · host ${fmtBytes(st.mem_available)} available, reserve ${fmtBytes(st.mem_reserve)}`)),
  ];
  for (const d of m?.disk || []) {
    const used = d.total ? 1 - d.free / d.total : 0;
    rows.push(h("div", { class: "meter-row" }, h("span", { class: "k" }, d.name === "state" ? "Disk" : "Images"), meter(used, used > 0.9 ? "crit" : used > 0.8 ? "warn" : ""),
      h("span", { class: "v num" }, `${fmtBytes(d.free)} free of ${fmtBytes(d.total)}`)));
  }
  const foot = h("div", { class: "runner-foot" },
    h("span", {}, h("b", {}, st.running), " running"), h("span", {}, h("b", {}, st.queued), " queued"),
    h("span", {}, h("b", {}, st.sessions), " session" + (st.sessions === 1 ? "" : "s")),
    st.images ? h("span", {}, h("b", {}, st.images), " images") : null,
    h("span", {}, h("b", {}, (st.warm_keys || []).length), " warm workspaces"),
    (st.features || []).length ? h("span", {}, "cgroups: " + st.features.join(", ")) : null);
  return h("div", { class: "card" }, head, h("div", { class: "runner-meta" }, meta.filter(Boolean).join(" · ")), cores, rows, foot);
}

function pauseBtn(r) {
  const paused = !!r.status.paused, busy = state.pausing.get(r.name) === "";
  const label = (paused ? "Resume " : "Pause ") + r.name;
  const glyph = paused ? s("path", { d: "M5.5 3.5v9l7-4.5z", fill: "currentColor", "stroke-linejoin": "round" })
    : s("path", { d: "M5.5 3.5v9M10.5 3.5v9", stroke: "currentColor", "stroke-width": 2.2, "stroke-linecap": "round" });
  return h("button", { class: "icon-btn", type: "button", "data-pause": r.name, "aria-label": label, title: paused ? label : label + ": refuse new jobs and sessions, finish the rest", "aria-disabled": busy ? "true" : null, onclick: async () => {
    if (state.pausing.get(r.name) === "") return;
    state.pausing.set(r.name, ""); render();
    try {
      const res = await api("/api/pause", { method: "POST", headers: { "Content-Type": "application/json", "X-Offload-Dashboard": "1" }, body: JSON.stringify({ host: r.name, paused: !paused }) });
      state.pausing.delete(r.name);
      const cur = state.fleet?.runners.find((x) => x.name === r.name);
      if (cur) cur.status = { ...res, allocated: { cpu: 0, mem: 0, ...res.allocated } };
    } catch (e) {
      const msg = `couldn't ${paused ? "resume" : "pause"} it: ${e.message}`;
      state.pausing.set(r.name, msg);
      setTimeout(() => { if (state.pausing.get(r.name) === msg) { state.pausing.delete(r.name); render(); } }, 10000);
    }
    render();
  } }, s("svg", { viewBox: "0 0 16 16", width: 16, height: 16, "aria-hidden": "true" }, glyph));
}

function renderQueue(runners) {
  const el = $("#ov-queue");
  const waiting = [];
  for (const r of runners) if (r.online) for (const j of r.jobs) if (j.state === "queued" || j.state === "uploading") waiting.push([r, j]);
  if (!waiting.length) { el.replaceChildren(); return; }
  waiting.sort((a, b) => new Date(a[1].created_at) - new Date(b[1].created_at));
  el.replaceChildren(
    h("div", { class: "section-head" }, h("h2", {}, "Waiting"), h("span", { class: "muted" }, `${waiting.length} job${waiting.length === 1 ? "" : "s"} not started yet`)),
    h("div", { class: "table-wrap" }, h("table", {},
      h("thead", {}, h("tr", {}, ["Job", "Command", "Runner", "Waiting", "Asks for", "Why"].map((t) => h("th", {}, t)))),
      h("tbody", {}, waiting.map(([r, j]) => h("tr", { class: "click", tabindex: 0, onclick: () => (location.hash = `#/containers/${encodeURIComponent(r.name)}/${j.id}`) },
        h("td", { class: "mono" }, j.id), h("td", {}, h("span", { class: "cmd mono" }, jobCmd(j))), h("td", {}, r.name),
        h("td", { class: "num nowrap" }, fmtDur(since(j.created_at))),
        h("td", { class: "num nowrap" }, `${fmtCores(j.resources.cpu)} cores · ${fmtBytes(j.resources.mem)}`),
        h("td", { class: "hint" }, (j.reason || (j.state === "uploading" ? "receiving source files" : "")).replace(/^waiting: /, ""))))))));
}

// ---------- charts ----------
const SERIES = {
  cpu: [["cpu_busy", "Whole host busy", "--series-1"], ["cpu_used", "Used by offload", "--series-2"], ["cpu_alloc", "Guaranteed to work", "--series-3"]],
  mem: [["mem_used", "Whole host in use", "--series-1"], ["mem_work", "Used by offload", "--series-2"], ["mem_alloc", "Booked", "--series-3"]],
};

// combine sums every runner's series into one, aligned on step buckets. Within a runner, points
// in a bucket are averaged weighted by n (the samples each stands for; absent means 1).
function combine(seriesByRunner) {
  const all = Object.values(seriesByRunner).filter((x) => x && x.points.length);
  if (!all.length) return null;
  const step = Math.max(...all.map((x) => x.step));
  const buckets = new Map();
  for (const sr of all) {
    const per = new Map();
    for (const p of sr.points) {
      const b = Math.floor(p.t / step) * step;
      const acc = per.get(b) || { w: 0, v: {} };
      const w = p.n || 1;
      for (const k of Object.keys(p)) if (k !== "t" && k !== "n") acc.v[k] = (acc.v[k] || 0) + w * p[k];
      acc.w += w;
      per.set(b, acc);
    }
    for (const [b, acc] of per) {
      const tot = buckets.get(b) || { t: b };
      for (const [k, v] of Object.entries(acc.v)) tot[k] = (tot[k] || 0) + v / acc.w;
      buckets.set(b, tot);
    }
  }
  return { step, points: [...buckets.values()].sort((a, b) => a.t - b.t) };
}

function renderCharts() {
  const el = $("#ov-charts");
  if (!el) return;
  const runners = metricRunners();
  const st = runners.map((r) => r.status);
  const loading = state.seriesFor !== state.range + "|" + state.runner;
  const data = loading ? null : combine(state.series); // never another filter's data under these labels
  const capCPU = sum(st, (x) => x.capacity.cpu), hostCPUs = sum(st, (x) => x.host_cpus);
  const capMem = sum(st, (x) => x.capacity.mem), memTotal = sum(runners, (r) => r.metrics?.mem_total);
  const sub = $("#ov-charts-sub");
  const left = (state.fleet?.runners || []).filter((r) => !r.current && r.status?.metrics !== false && (!state.runner || r.name === state.runner)).map((r) => r.name);
  if (sub) sub.textContent = (state.runner ? state.runner : runners.length > 1 ? `All ${runners.length} runners, added together` : "") +
    (left.length ? `${state.runner || runners.length > 1 ? " · " : ""}left out (not answering): ${left.join(", ")}` : "");
  const cards = [
    chartCard("CPU", "Cores", data, SERIES.cpu, { ref: capCPU, refLabel: `capacity ${fmtCores(capCPU)}`, max: Math.max(hostCPUs, capCPU), fmt: fmtCores, unit: " cores" }, loading),
    chartCard("Memory", "Bytes", data, SERIES.mem, { ref: capMem, refLabel: `capacity ${fmtBytes(capMem)}`, max: Math.max(memTotal, capMem), fmt: fmtBytes, unit: "" }, loading),
  ];
  // Keep the previous render while a refetch runs (no flash): swap only when data is in.
  el.replaceChildren(...cards);
}

function chartCard(title, unit, data, series, o, loading) {
  const box = h("div", { class: "chart" + (loading ? " loading" : "") });
  const card = h("div", { class: "card chart-card" },
    h("h3", {}, title),
    h("div", { class: "legend" },
      series.map(([, label, color]) => h("span", {}, h("i", { style: { background: `var(${color})` } }), label)),
      o.ref ? h("span", {}, h("i", { class: "ref" }), "Capacity offload hands out") : null),
    box);
  requestAnimationFrame(() => drawChart(box, data, series, { ...o, loading }));
  return card;
}

// niceMax rounds an axis top up to a value whose quarters are round numbers (12 -> 12, 13 -> 16).
function niceMax(v) {
  if (!(v > 0)) return 1;
  const e = Math.pow(10, Math.floor(Math.log10(v))), f = v / e;
  return ([1, 1.2, 1.6, 2, 2.4, 3, 4, 5, 6, 8, 10].find((x) => x >= f - 1e-9) || 10) * e;
}
function niceBytesMax(v) {
  if (!(v > 0)) return GiB;
  const g = v / GiB;
  const steps = [1, 2, 4, 8, 16, 24, 32, 48, 64, 96, 128, 192, 256, 384, 512, 768, 1024, 1536, 2048];
  return (steps.find((x) => x >= g) || Math.ceil(g / 1024) * 1024) * GiB;
}

function drawChart(box, data, series, o) {
  const W = box.clientWidth || 600, H = box.clientHeight || 220;
  const m = { l: 52, r: W < 560 ? 76 : 112, t: 10, b: 24 };
  const svg = s("svg", { viewBox: `0 0 ${W} ${H}`, role: "img", "aria-label": "chart" });
  if (!data || data.points.length < 2) {
    svg.append(s("text", { x: W / 2, y: H / 2, "text-anchor": "middle", class: "empty" }, data ? "Collecting samples…" : o.loading ? "Loading…" : "No usage data yet"));
    box.replaceChildren(svg);
    return;
  }
  const pts = data.points;
  const now = Date.now() / 1000;
  const x0 = Math.min(pts[0].t, now - rangeSecs()), x1 = Math.max(pts[pts.length - 1].t, now - 5);
  const isBytes = o.fmt === fmtBytes;
  let ymax = Math.max(o.max || 0, ...pts.flatMap((p) => series.map(([k]) => p[k] || 0)));
  ymax = isBytes ? niceBytesMax(ymax) : niceMax(ymax);
  const X = (t) => m.l + ((t - x0) / (x1 - x0)) * (W - m.l - m.r);
  const Y = (v) => m.t + (1 - v / ymax) * (H - m.t - m.b);
  // grid + y ticks
  for (let i = 0; i <= 4; i++) {
    const v = (ymax * i) / 4, y = Math.round(Y(v)) + 0.5;
    svg.append(s("line", { x1: m.l, x2: W - m.r, y1: y, y2: y, class: i === 0 ? "baseline" : "gridline" }));
    svg.append(s("text", { x: m.l - 8, y: y + 4, "text-anchor": "end", class: "tick" }, o.fmt(v)));
  }
  // x ticks
  const span = x1 - x0;
  // x ticks at round times, at least ~84px apart
  const plotW = W - m.l - m.r;
  const stepX = [300, 600, 900, 1800, 3600, 7200, 3 * 3600, 6 * 3600, 12 * 3600, 86400, 2 * 86400].find((st) => (st / span) * plotW >= 84) || 7 * 86400;
  const tz = new Date().getTimezoneOffset() * 60;
  for (let t = Math.ceil((x0 - tz) / stepX) * stepX + tz; t <= x1; t += stepX) {
    const d = new Date(t * 1000);
    const label = stepX >= 86400 ? d.toLocaleDateString([], { weekday: "short", day: "numeric" }) : d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    svg.append(s("text", { x: X(t), y: H - 6, "text-anchor": "middle", class: "tick" }, label));
  }
  // capacity reference line
  if (o.ref) {
    const y = Math.round(Y(o.ref)) + 0.5;
    svg.append(s("line", { x1: m.l, x2: W - m.r, y1: y, y2: y, class: "ref" }));
  }
  // lines, broken across gaps (a runner that was down)
  const gap = data.step * 3;
  const ends = [];
  for (const [k, label, color] of series) {
    let d = "", prev = null;
    for (const p of pts) {
      if (p[k] == null) continue;
      d += (prev == null || p.t - prev > gap ? "M" : "L") + X(p.t).toFixed(1) + "," + Y(p[k]).toFixed(1);
      prev = p.t;
    }
    svg.append(s("path", { d, class: "series", style: { stroke: `var(${color})` } }));
    const last = pts[pts.length - 1];
    ends.push({ y: Y(last[k] || 0), text: o.fmt(last[k] || 0), label, color, x: X(last.t) });
  }
  // direct end labels, unless they would collide (then the legend and tooltip carry them)
  const ys = ends.map((e) => e.y).sort((a, b) => a - b);
  const collide = ys.some((y, i) => i && y - ys[i - 1] < 13);
  for (const e of ends) {
    svg.append(s("circle", { cx: e.x, cy: e.y, r: 4, class: "dot", style: { fill: `var(${e.color})` } }));
    if (!collide) svg.append(s("text", { x: e.x + 9, y: e.y + 4, class: "end-label" }, e.text));
  }
  if (o.ref) {
    const y = Y(o.ref);
    const near = ends.some((e) => Math.abs(e.y - y) < 13);
    if (!near || collide) svg.append(s("text", { x: W - m.r + 9, y: y + 4, class: "ref-label" }, o.refLabel));
  }
  // crosshair + one tooltip for every series
  const cross = s("line", { y1: m.t, y2: H - m.b, class: "crosshair", visibility: "hidden" });
  const dots = series.map(([, , color]) => s("circle", { r: 4, class: "dot", visibility: "hidden", style: { fill: `var(${color})` } }));
  const hit = s("rect", { x: m.l, y: 0, width: W - m.l - m.r, height: H, fill: "transparent", tabindex: 0, "aria-label": "Move along the chart to read values" });
  svg.append(cross, ...dots, hit);
  let idx = pts.length - 1;
  const show = (cx, cy) => {
    const p = pts[idx], x = X(p.t);
    cross.setAttribute("x1", x); cross.setAttribute("x2", x); cross.setAttribute("visibility", "visible");
    series.forEach(([k], i) => { dots[i].setAttribute("cx", x); dots[i].setAttribute("cy", Y(p[k] || 0)); dots[i].setAttribute("visibility", "visible"); });
    showTip(cx, cy, [h("div", { class: "t" }, fmtTime(p.t * 1000)),
      ...series.map(([k, label, color]) => h("div", { class: "r" }, h("i", { style: { background: `var(${color})` } }), h("b", {}, o.fmt(p[k] || 0)), h("span", {}, label))),
      o.ref ? h("div", { class: "r" }, h("i", { style: { background: "var(--axis)", height: "1px" } }), h("b", {}, o.fmt(o.ref)), h("span", {}, "capacity")) : null]);
  };
  const nearest = (clientX) => {
    const r = svg.getBoundingClientRect();
    const t = x0 + ((clientX - r.left) * (W / r.width) - m.l) / (W - m.l - m.r) * (x1 - x0);
    let best = 0;
    for (let i = 1; i < pts.length; i++) if (Math.abs(pts[i].t - t) < Math.abs(pts[best].t - t)) best = i;
    return best;
  };
  hit.addEventListener("pointermove", (e) => { idx = nearest(e.clientX); show(e.clientX, e.clientY); });
  const hide = () => { cross.setAttribute("visibility", "hidden"); dots.forEach((d) => d.setAttribute("visibility", "hidden")); hideTip(); };
  hit.addEventListener("pointerleave", hide);
  hit.addEventListener("blur", hide);
  hit.addEventListener("keydown", (e) => {
    if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
    idx = Math.max(0, Math.min(pts.length - 1, idx + (e.key === "ArrowLeft" ? -1 : 1)));
    const r = svg.getBoundingClientRect();
    show(r.left + X(pts[idx].t) * (r.width / W), r.top + 20);
    e.preventDefault();
  });
  box.replaceChildren(svg);
}

// ---------- activity (ledger) ----------
function renderActivity() {
  const el = $("#ov-activity");
  if (!el) return;
  const label = { "1h": "last hour", "6h": "last 6 hours", "24h": "last 24 hours", "7d": "last 7 days" }[state.range];
  const head = h("div", { class: "section-head" }, h("h2", {}, "Activity"), h("span", { class: "muted" }, "Finished work, " + label + (state.runner ? " on " + state.runner : "")));
  if (!state.ledger || state.ledgerFor !== state.range) { el.replaceChildren(head, h("div", { class: "card empty-state" }, "Loading…")); return; }
  if (state.runner && state.ledgerErrors?.[state.runner]) {
    el.replaceChildren(head, h("div", { class: "card empty-state" }, `History unavailable: ${state.runner} is ${state.ledgerErrors[state.runner].replace(/, so its history is left out$/, "")}.`));
    return;
  }
  const rows = state.ledger.filter((e) => !state.runner || e.runner === state.runner);
  const jobs = rows.filter((e) => e.kind === "job");
  const errs = state.ledgerErrors ? Object.entries(state.ledgerErrors).map(([k, v]) => h("div", { class: "note" }, `${k}: ${v}`)) : [];
  const cut = state.ledgerTruncated.filter((n) => !state.runner || n === state.runner);
  if (cut.length) errs.push(h("div", { class: "note" }, `Partial: ${cut.join(", ")} sent only its newest 50,000 entries for this range, so these statistics leave out older work.`));
  if (!jobs.length) {
    el.replaceChildren(head, h("div", { class: "card empty-state" }, rows.length ? "Only sessions finished in this range." : "No jobs finished in this range."), errs);
    return;
  }
  const ok = jobs.filter((e) => e.state === "exited" && e.exit_code === 0).length;
  const oom = jobs.filter((e) => e.oom).length;
  const started = jobs.filter((e) => e.started_at);
  const waits = started.map((e) => e.queue_wait);
  const cpuH = sum(jobs, (e) => e.cpu_seconds) / 3600;
  const warm = jobs.filter((e) => e.sync_files > 0 && e.uploaded != null); // the sync finished
  const warmRate = warm.length ? 1 - sum(warm, (e) => e.uploaded) / Math.max(1, sum(warm, (e) => e.sync_files)) : null;
  const stats = h("div", { class: "stats" },
    kpi("Jobs finished", String(jobs.length), `${pct(ok / jobs.length)} succeeded`),
    kpi("Out of memory", String(oom), oom ? "Retried once with double memory when the limit was guessed" : "No job hit its memory limit"),
    kpi("Wait before start", fmtDur(quantile(waits, 0.5)), `median · 95th percentile ${fmtDur(quantile(waits, 0.95))}`),
    kpi("CPU time used", cpuH < 10 ? cpuH.toFixed(1) + " h" : Math.round(cpuH) + " h", `across ${started.length} started job${started.length === 1 ? "" : "s"}`),
    warmRate != null ? kpi("Files already warm", pct(warmRate), "of synced files didn't need uploading") : null,
  );
  // Per command
  const by = new Map();
  for (const e of jobs) { const k = e.command || "?"; (by.get(k) || by.set(k, []).get(k)).push(e); }
  const cmds = [...by.entries()].map(([cmd, es]) => {
    const ran = es.filter((e) => e.started_at && e.duration > 0);
    const guaranteed = sum(ran, (e) => e.resources.cpu * e.duration);
    const used = sum(ran, (e) => e.cpu_seconds);
    const avgCores = ran.length ? used / Math.max(1, sum(ran, (e) => e.duration)) : null;
    const peakFrac = Math.max(0, ...es.filter((e) => e.peak_mem && e.resources.mem).map((e) => e.peak_mem / e.resources.mem));
    return {
      cmd, runs: es.length, ok: es.filter((e) => e.state === "exited" && e.exit_code === 0).length, oom: es.filter((e) => e.oom).length,
      p50: quantile(ran.map((e) => e.duration), 0.5), p95: quantile(ran.map((e) => e.duration), 0.95), cpu: used,
      eff: guaranteed > 0 && used > 0 ? used / guaranteed : null, avgCores, asks: quantile(ran.map((e) => e.resources.cpu), 0.5),
      peakFrac: peakFrac || null, mem: quantile(es.map((e) => e.resources.mem), 0.5),
    };
  }).sort((a, b) => b.cpu - a.cpu || b.runs - a.runs).slice(0, 12);
  const hint = (c) => {
    if (c.oom) return [h("b", {}, `${c.oom} out of memory`), "; its limit is learned from peaks and grows after each one"];
    if (c.eff != null && c.eff < 0.35 && c.runs >= 2 && c.asks >= 2) return ["Asks for ", h("b", {}, fmtCores(c.asks) + " cores"), ", uses ", h("b", {}, fmtCores(c.avgCores)), " on average"];
    if (c.peakFrac != null && c.peakFrac > 0.9) return ["Peaks at ", h("b", {}, pct(c.peakFrac)), " of its memory limit"];
    return "";
  };
  const table = h("div", { class: "table-wrap" }, h("table", {},
    h("thead", {}, h("tr", {}, h("th", {}, "Command"), h("th", { class: "r" }, "Runs"), h("th", { class: "r" }, "Succeeded"), h("th", { class: "r" }, "Median time"),
      h("th", { class: "r" }, "95th pct"), h("th", { class: "r" }, "CPU time"), h("th", { class: "r" }, "CPU used of guaranteed"), h("th", { class: "r" }, "Peak memory of limit"), h("th", {}, "Note"))),
    h("tbody", {}, cmds.map((c) => h("tr", {},
      h("td", {}, h("span", { class: "cmd mono" }, c.cmd)),
      h("td", { class: "r num" }, c.runs), h("td", { class: "r num" }, pct(c.ok / c.runs)),
      h("td", { class: "r num" }, fmtDur(c.p50)), h("td", { class: "r num" }, fmtDur(c.p95)),
      h("td", { class: "r num" }, c.cpu ? fmtDur(c.cpu) : "–"),
      h("td", { class: "r num" }, c.eff == null ? "–" : pct(c.eff)),
      h("td", { class: "r num" }, c.peakFrac == null ? "–" : pct(c.peakFrac) + " of " + fmtBytes(c.mem)),
      h("td", { class: "hint" }, hint(c)))))));
  el.replaceChildren(head, stats, h("div", { class: "section-head" }, h("h2", {}, "By command"), h("span", { class: "muted" }, "Most CPU time first")), table, errs);
}

// ---------- containers ----------
function workloads() {
  const out = [];
  for (const r of state.fleet?.runners || []) {
    const usage = new Map((r.metrics?.workloads || []).map((w) => [w.id, w]));
    const owners = new Map();
    for (const c of r.metrics?.cpus || []) for (const id of c.owners || []) (owners.get(id) || owners.set(id, []).get(id)).push(c.id);
    for (const j of r.jobs) out.push({
      kind: "job", runner: r.name, id: j.id, state: j.state, reason: j.reason, exit_code: j.exit_code, cmd: jobCmd(j), github: j.spec.github ? j.github || { owner: "" } : null,
      repo: j.spec.github ? j.github?.repo || "" : shortKey(j.spec.workspace_key), caller: j.caller || j.spec.client || "", res: j.resources, cpus: j.allowed_cpus,
      created: j.created_at, started: j.started_at, finished: j.finished_at, runtime: j.runtime, image: j.image,
      use: usage.get(j.id), raw: j, host: r,
    });
    for (const se of r.sessions) out.push({
      kind: "session", runner: r.name, id: se.id, state: se.state, reason: se.reason, exit_code: se.exit_code, cmd: se.profile,
      repo: "", caller: se.caller || se.client || "", res: se.resources, cpus: compactCPUs(owners.get(se.id)),
      created: se.created_at, started: se.created_at, finished: se.finished_at, runtime: se.runtime, image: se.image,
      use: usage.get(se.id), raw: se, host: r,
    });
  }
  return out;
}
function compactCPUs(list) {
  if (!list || !list.length) return "";
  const a = [...list].sort((x, y) => x - y), parts = [];
  for (let i = 0; i < a.length; ) { let j = i; while (j + 1 < a.length && a[j + 1] === a[j] + 1) j++; parts.push(i === j ? a[i] : a[i] + "-" + a[j]); i = j + 1; }
  return parts.join(",");
}
const isActive = (w) => !["exited", "failed", "cancelled"].includes(w.state);
const stale = (w) => isActive(w) && !w.host.online;
const live = (w) => isActive(w) && w.host.online;

let ctFiltersKey = "";
function renderContainers() {
  const main = $("#main");
  if (mainBuilt !== "containers") {
    mainBuilt = "containers";
    const f = state.cfilter;
    main._search = h("input", { type: "search", placeholder: "Filter by command, id, repo, caller…", "aria-label": "Filter", value: f.q,
      oninput: (e) => { f.q = e.target.value; renderTable(); } });
    main.replaceChildren(h("div", { class: "filters", id: "ct-filters" }), h("div", { id: "ct-table" }));
    ctFiltersKey = "";
  }
  if (ctFiltersKey !== runnerNames()) renderCtFilters();
  renderTable();
}
function renderCtFilters() {
  const f = state.cfilter;
  const again = () => { renderCtFilters(); renderTable(); };
  ctFiltersKey = runnerNames();
  $("#ct-filters").replaceChildren(...[
    segmented([["active", "Active"], ["finished", "Finished"], ["all", "All"]], f.state, (v) => { f.state = v; store("cstate", v); again(); }, "State"),
    segmented([["", "All kinds"], ["job", "Jobs"], ["session", "Sessions"], ["github", "GitHub"]], f.kind, (v) => { f.kind = v; again(); }, "Kind"),
    runnerSelect(f.runner, (v) => { f.runner = v; again(); }),
    $("#main")._search,
    f.state !== "active" ? h("span", { class: "muted", style: { fontSize: "12.5px" } }, "Runners list finished work for an hour; Overview › Activity goes further back") : null,
  ].filter(Boolean));
}

// usageCell shows live use against a guarantee or limit; warn marks a hard limit (memory).
function usageCell(used, limit, fmt, unitText, warn) {
  if (used == null) return h("span", { class: "muted num" }, limit ? fmt(limit) + unitText : "–");
  const frac = limit ? used / limit : 0;
  return h("div", { class: "usage" }, meter(frac, warn && frac > 0.9 ? "warn" : ""), h("span", { class: "t num" }, fmt(used), h("span", { class: "muted" }, " / " + fmt(limit) + unitText)));
}

function renderTable() {
  const el = $("#ct-table");
  if (!el) return;
  const f = state.cfilter, q = f.q.trim().toLowerCase();
  const kind = (w) => (w.github ? "github" : w.kind);
  let list = workloads().filter((w) =>
    (f.state === "all" || (f.state === "active") === isActive(w)) && (!f.kind || kind(w) === f.kind) && (!f.runner || w.runner === f.runner) &&
    (!q || [w.id, w.cmd, w.repo, w.caller, w.runner, w.state].some((x) => (x || "").toLowerCase().includes(q))));
  list.sort((a, b) => (isActive(b) - isActive(a)) || new Date(b.created) - new Date(a.created));
  if (!list.length) {
    const msg = !state.fleet?.runners.length ? "No runners found yet." : f.state === "active" ? "Nothing is running. Jobs you start with offload run, sessions and GitHub jobs show up here." : "No workloads match.";
    el.replaceChildren(h("div", { class: "card empty-state" }, msg));
    return;
  }
  const sel = state.sel;
  const showRunner = (state.fleet?.runners || []).length > 1, showCPUs = list.some((w) => w.cpus);
  el.replaceChildren(h("div", { class: "table-wrap" }, h("table", {},
    h("thead", {}, h("tr", {}, ["Workload", "Command", showRunner && "Runner", "State", "CPU used (cores)", "Memory", showCPUs && "Pinned CPUs", "Repository", "Caller", "Time"].filter(Boolean).map((t) => h("th", {}, t)))),
    h("tbody", {}, list.map((w) => {
      const open = () => (location.hash = `#/containers/${encodeURIComponent(w.runner)}/${w.id}`);
      const t = stale(w) ? (w.host.last_seen ? `as of ${ago(w.host.last_seen)}` : "") : w.finished ? `ended ${ago(w.finished)}` : w.started && w.state === "running" ? `for ${fmtDur(since(w.started))}` : `waiting ${fmtDur(since(w.created))}`;
      return h("tr", { class: "click" + (sel && sel.id === w.id && sel.runner === w.runner ? " sel" : ""), tabindex: 0, onclick: open, onkeydown: (e) => { if (e.key === "Enter") open(); } },
        h("td", { class: "nowrap" }, h("span", { class: "kind" }, w.github ? "GitHub" : w.kind === "job" ? "Job" : "Session"), h("span", { class: "mono" }, w.id)),
        h("td", {}, h("span", { class: "cmd mono", title: w.cmd }, w.cmd)),
        showRunner && h("td", { class: "nowrap" }, w.runner),
        h("td", {}, w.github ? ghStatus(w) : exitChip(w)),
        h("td", {}, live(w) ? usageCell(w.use?.cpu, w.res.cpu, fmtCores, " guaranteed") : h("span", { class: "muted num" }, w.raw.cpu_seconds ? fmtDur(w.raw.cpu_seconds) + " CPU" : "–")),
        h("td", {}, live(w) ? usageCell(w.use?.mem, w.res.mem, fmtBytes, " limit", true) : h("span", { class: "muted num" }, w.raw.peak_mem ? "peak " + fmtBytes(w.raw.peak_mem) : "–")),
        showCPUs && h("td", { class: "mono nowrap" }, w.cpus || "–"),
        h("td", { class: "nowrap" }, w.repo ? truncate(w.repo, 28) : h("span", { class: "muted" }, "–")),
        h("td", { class: "nowrap" }, w.caller || h("span", { class: "muted" }, "–")),
        h("td", { class: "nowrap num muted" }, t));
    })))));
}

// ---------- GitHub ----------
const workloadHash = (r, id) => `#/${r.jobs.some((j) => j.id === id && j.spec.github) ? "github" : "containers"}/${encodeURIComponent(r.name)}/${encodeURIComponent(id)}`;
const ghPool = (r, owner) => (r.github || []).find((p) => p.owner.toLowerCase() === (owner || "").toLowerCase());
const ghSettings = (owner) => owner.includes("/") ? `https://github.com/${owner}/settings/actions/runners` : `https://github.com/organizations/${owner}/settings/actions/runners`;
const agoCoarse = (t) => { const x = since(t); return x < 60 ? "just now" : x < 3600 ? Math.round(x / 60) + "m ago" : fmtDur(x) + " ago"; };
const GH_ARCHS = ["x64", "arm64", "arm"];
const ghLabels = (p) => {
  const out = [], seen = new Set();
  for (const l of [...(p.system_labels || []), ...(p.labels || [])]) if (!seen.has(l.toLowerCase())) { seen.add(l.toLowerCase()); out.push(l); }
  return out;
};
const labelChips = (labels) => labels.map((l, i) => [i ? " " : "", h("code", { class: "label" }, l)]).flat();
const ghArch = (r) => ({ amd64: "x64", arm64: "arm64", arm: "arm" })[r.status?.arch] || r.status?.arch || "";
// The labels chosen runners start with: GitHub's usual ones and offload, or what the owner's runners
// share. An architecture label is the chosen runners' when they all share one, and left out if not.
function ghDefaultLabels(runners, base) {
  const archs = [...new Set(runners.map(ghArch).filter(Boolean))];
  const arch = archs.length === 1 ? archs : [];
  if (!base) return ["self-hosted", "linux", ...arch, "offload"];
  return base.flatMap((l) => GH_ARCHS.includes(l.toLowerCase()) ? arch : [l]);
}
const runsOn = (labels) => (!labels?.length ? "offload" : labels.length === 1 ? labels[0] : "[" + labels.join(", ") + "]");
const extIcon = () => glyphSVG(s("path", { d: "M6.5 3.5H4A1.5 1.5 0 0 0 2.5 5v7A1.5 1.5 0 0 0 4 13.5h7a1.5 1.5 0 0 0 1.5-1.5V9.5M9 2.5h4.5V7M13.5 2.5 7.5 8.5" }));
const copyIcon = () => glyphSVG(s("rect", { x: "5.25", y: "5.25", width: "8.5", height: "8.5", rx: "1.5" }), s("path", { d: "M10.75 5.25V3.75a1.5 1.5 0 0 0-1.5-1.5h-5.5a1.5 1.5 0 0 0-1.5 1.5v5.5a1.5 1.5 0 0 0 1.5 1.5h1.5" }));
const checkIcon = () => glyphSVG(s("path", { d: "M3.5 8.5 6.5 11.5 12.5 4.5" }));

function ghStatus(w) {
  const g = w.github, r = w.raw;
  if (stale(w)) return exitChip(w);
  const verdict = { succeeded: chip("good", "Succeeded"), failed: chip("critical", "Failed"), canceled: chip("neutral", "Cancelled"), cancelled: chip("neutral", "Cancelled") }[g.result];
  if (isActive(w)) {
    if (verdict) return verdict; // GitHub has answered; the container is still stopping
    if (g.job) return chip("running", "Running");
    return chip("neutral", w.state === "queued" && (r.reason || "").includes("building") ? "Building env" : "Starting");
  }
  if (r.reason && r.reason.startsWith("out of memory")) return chip("critical", "Out of memory");
  if (verdict) return verdict;
  if (!g.job) return w.state === "cancelled" ? chip("neutral", "Stopped") : w.state === "failed" || w.exit_code ? exitChip(w) : chip("neutral", "Took no job");
  return w.state === "failed" ? chip("critical", "Failed") : chip("neutral", "Finished");
}
function poolChip(p) {
  switch (p.state) {
    case "listening": return chip("good", "Ready");
    case "preparing": return chip("info", "Preparing");
    case "failed": return chip("critical", "Not taking jobs");
    case "removing": return chip("neutral", "Removing");
  }
  return chip("neutral", p.state || "?");
}
function copyBtn(text, label) {
  const done = state.copied === text;
  return h("button", { type: "button", class: "btn copy" + (done ? " done" : ""), "aria-label": done ? "Copied" : "Copy " + (label || text), onclick: async (e) => {
    e.stopPropagation();
    try { await navigator.clipboard.writeText(text); } catch { return; }
    state.copied = text; render();
    setTimeout(() => { if (state.copied === text) { state.copied = ""; render(); } }, 1600);
  } }, done ? checkIcon() : copyIcon(), done ? "Copied" : "Copy");
}
function slots(p) {
  const busy = p.running, starting = p.starting, total = Math.max(p.capacity, busy + starting);
  const room = Math.max(0, total - busy - starting);
  const label = [busy && `${busy} running`, starting && `${starting} starting`, `room for ${room} more`].filter(Boolean).join(", ");
  if (total > 32 || p.state !== "listening") return null;
  const cells = [];
  for (let i = 0; i < total; i++) cells.push(h("i", { class: "cell " + (i < busy ? "h4" : i < busy + starting ? "h1" : "") }));
  return h("div", { class: "slots", role: "img", "aria-label": label, title: label }, cells);
}
function roomText(p) {
  if (p.state !== "listening") return p.detail || "";
  const busy = p.running + p.starting, room = Math.max(0, p.capacity - busy);
  if (!busy && !room) return "Full right now: GitHub holds its jobs until there's room";
  return [p.running && `${p.running} running`, p.starting && `${p.starting} starting`, room ? `room for ${room} more` : "no room for more"].filter(Boolean).join(" · ");
}

function ghWorkloads() {
  return workloads().filter((w) => w.github);
}
function renderTabCounts() {
  const a = document.querySelector('.tabs a[data-view="github"]');
  if (!a) return;
  const n = ghWorkloads().filter((w) => live(w) && w.github.job).length;
  let c = a.querySelector(".count");
  if (!n) { c?.remove(); a.removeAttribute("aria-label"); return; }
  if (!c) { c = h("span", { class: "count", "aria-hidden": "true" }); a.append(c); }
  c.textContent = n;
  a.setAttribute("aria-label", `GitHub, ${n} job${n === 1 ? "" : "s"} running`);
}

// The GitHub view is a fixed skeleton: its filters and the add form are built once and kept, so a
// refresh never takes focus or typing away; only the live parts are redrawn.
let ghFiltersKey = "";
const plusIcon = () => glyphSVG(s("path", { d: "M8 3v10M3 8h10" }));
const ghAlert = (kind, ...kids) => h("div", { class: "alert " + kind, role: kind === "critical" ? "alert" : null }, icon(kind), h("div", {}, kids));

function ghRunners() {
  // Runners that can take GitHub jobs: answering, current, with containers, and not this machine's own.
  return (state.fleet?.runners || []).filter((r) => r.online && r.current && r.status?.runtime === "container" && r.url !== "http://local");
}

function renderGitHubView() {
  const main = $("#main");
  if (mainBuilt !== "github") {
    mainBuilt = "github"; ghFiltersKey = "";
    main.replaceChildren(
      h("div", { class: "alerts", id: "gh-alerts" }),
      h("section", { id: "gh-jobs" }, h("div", { class: "section-head" }, h("h2", {}, "Jobs")), h("div", { class: "filters", id: "gh-filters" }), h("div", { id: "gh-jobs-body" })),
      h("section", { id: "gh-owners" },
        h("div", { class: "section-head" }, h("h2", {}, "Organizations and repositories"), h("span", { class: "muted", id: "gh-owners-sub" }),
          h("button", { type: "button", class: "btn primary push", id: "gh-add-btn", onclick: () => openGhForm() }, plusIcon(), "Add organization")),
        ghFormEl(), h("div", { id: "gh-owners-body" })));
  }
  if (!state.fleet) {
    $("#gh-jobs-body").replaceChildren(h("div", { class: "card empty-state" }, state.fleetErr || "Loading…"));
    return;
  }
  const owners = new Map(), spelled = new Map(); // GitHub names ignore case: one card per owner
  for (const r of state.fleet.runners) for (const p of r.github || []) {
    const k = p.owner.toLowerCase();
    if (!spelled.has(k)) { spelled.set(k, p.owner); owners.set(p.owner, []); }
    owners.get(spelled.get(k)).push([r, p]);
  }
  const alerts = [];
  if (state.ghNotice) alerts.push(ghAlert(state.ghNotice.kind, state.ghNotice.text));
  for (const [owner, list] of owners) for (const [r, p] of list) {
    if (p.state === "failed") alerts.push(ghAlert("critical", h("b", {}, r.name), ` isn't taking ${owner}'s jobs: ${p.detail || "unknown problem"}`));
    if (p.state === "removing" && (p.detail || "").startsWith("couldn't")) alerts.push(ghAlert("warning", h("b", {}, r.name), ": " + p.detail));
    if (!r.online) alerts.push(ghAlert("warning", h("b", {}, r.name), ` isn't answering, so its ${owner} jobs below are what it last reported`));
  }
  $("#gh-alerts").replaceChildren(...alerts);
  const jobs = $("#gh-jobs");
  jobs.hidden = !owners.size && !ghWorkloads().length; // recent jobs stay reachable after the last removal
  if (!jobs.hidden) renderGitHubJobs([...owners.keys()].sort());
  $("#gh-owners-sub").textContent = owners.size ? "Every runner below tells GitHub how many jobs fit; GitHub sends each job to one with room" : "";
  $("#gh-add-btn").hidden = !owners.size || state.ghFormOpen;
  $("#gh-owners-body").replaceChildren(owners.size
    ? h("div", { class: "grid gh-owners" }, [...owners].sort((a, b) => a[0].localeCompare(b[0])).map(([owner, list]) => ownerCard(owner, list)))
    : state.ghFormOpen ? "" : ghOnboarding());
  state.ghOwners = owners;
  renderGhForm();
}

function renderGitHubJobs(owners) {
  const f = state.gfilter;
  if (f.owner && !owners.includes(f.owner)) f.owner = "";
  const key = owners.join("\n") + "|" + f.state + "|" + f.owner;
  if (key !== ghFiltersKey) {
    ghFiltersKey = key;
    $("#gh-filters").replaceChildren(...[
      segmented([["active", "Active"], ["finished", "Finished"], ["all", "All"]], f.state, (v) => { f.state = v; store("gstate", v); render(); }, "State"),
      owners.length > 1 ? h("select", { "aria-label": "Organization or repository", onchange: (e) => { f.owner = e.target.value; render(); } },
        h("option", { value: "" }, "Everywhere"), owners.map((o) => h("option", { value: o, selected: o === f.owner }, o))) : null,
      h("span", { class: "gh-summary", id: "gh-summary" }),
    ].filter(Boolean));
  }
  const all = ghWorkloads();
  const active = all.filter(isActive);
  const running = active.filter((w) => w.github.job).length, starting = active.length - running;
  let room = 0;
  for (const r of state.fleet.runners) if (r.online) for (const p of r.github || []) if (p.state === "listening") room += Math.max(0, p.capacity - p.running - p.starting);
  const done = all.filter((w) => !isActive(w) && w.github.job);
  const ok = done.filter((w) => w.github.result === "succeeded").length;
  $("#gh-summary").replaceChildren(...[
    h("b", {}, running), " running", starting ? [" · ", h("b", {}, starting), " starting"] : "",
    " · room for ", h("b", {}, room), " more",
    done.length ? [" · ", h("b", {}, done.length), " finished in the last hour, ", h("b", {}, pct(ok / done.length)), " succeeded"] : "",
  ].flat());
  const list = all.filter((w) => (f.state === "all" || (f.state === "active") === isActive(w)) && (!f.owner || (w.github.owner || "").toLowerCase() === f.owner.toLowerCase()))
    .sort((a, b) => (isActive(b) - isActive(a)) || new Date(b.created) - new Date(a.created));
  let body;
  if (!list.length) {
    body = h("div", { class: "card empty-state" }, f.state === "active"
      ? [h("p", {}, "No GitHub jobs right now."), h("p", {}, "They show up here the moment a workflow with ", h("code", {}, "runs-on: " + runsOn(ghFirstLabels())), " starts.")]
      : "No finished GitHub jobs in the last hour.");
  } else {
    const showRunner = state.fleet.runners.length > 1;
    body = h("div", { class: "table-wrap" }, h("table", {},
      h("thead", {}, h("tr", {}, ["Job", "Status", showRunner && "Runner", "Time", "CPU used (cores)", "Memory", ""].filter((t) => t !== false).map((t) => h("th", {}, t)))),
      h("tbody", {}, list.map((w) => {
        const g = w.github;
        const open = () => (location.hash = `#/github/${encodeURIComponent(w.runner)}/${w.id}`);
        const t = stale(w) ? (w.host.last_seen ? `as of ${ago(w.host.last_seen)}` : "") : w.finished ? (w.started ? `took ${fmtDur((new Date(w.finished) - new Date(w.started)) / 1000)}` : `ended ${ago(w.finished)}`)
          : w.started && w.state === "running" ? `for ${fmtDur(since(w.started))}` : `waiting ${fmtDur(since(w.created))}`;
        return h("tr", { class: "click" + (state.sel && state.sel.id === w.id && state.sel.runner === w.runner ? " sel" : ""), tabindex: 0, onclick: open, onkeydown: (e) => { if (e.key === "Enter") open(); } },
          h("td", { class: "gh-job" }, g.job
            ? [h("div", { class: "name" }, g.job), h("div", { class: "where" }, g.repo, g.workflow ? h("span", { class: "muted" }, " · " + g.workflow) : null)]
            : [h("div", { class: "name muted" }, isActive(w) ? "Waiting for a job" : "Took no job"), h("div", { class: "where muted" }, g.owner, h("span", { class: "mono" }, " · " + (g.runner || "")))]),
          h("td", {}, ghStatus(w)),
          showRunner && h("td", { class: "nowrap" }, w.runner),
          h("td", { class: "nowrap num muted" }, t),
          h("td", {}, live(w) && w.state === "running" ? usageCell(w.use?.cpu, w.res.cpu, fmtCores, " guaranteed") : h("span", { class: "muted num" }, w.raw.cpu_seconds ? fmtDur(w.raw.cpu_seconds) + " CPU" : "–")),
          h("td", {}, live(w) && w.state === "running" ? usageCell(w.use?.mem, w.res.mem, fmtBytes, " limit", true) : h("span", { class: "muted num" }, w.raw.peak_mem ? "peak " + fmtBytes(w.raw.peak_mem) : "–")),
          h("td", { class: "r" }, g.url ? h("a", { class: "icon-link", href: g.url, target: "_blank", rel: "noopener noreferrer", title: "Open the run on GitHub", "aria-label": `Open ${g.job} on GitHub`, onclick: (e) => e.stopPropagation() }, extIcon()) : null));
      }))));
  }
  $("#gh-jobs-body").replaceChildren(body);
}
const ghFirstLabels = () => { for (const r of state.fleet?.runners || []) for (const p of r.github || []) return p.labels; return null; };

// Two-step buttons (remove, forget): the first click arms it for a few seconds.
let ghArmTimer;
function armedBtn(key, label, confirmLabel, run) {
  const armed = state.ghArmed === key;
  return h("button", { type: "button", class: "btn danger" + (armed ? " confirm" : ""), disabled: state.ghBusyKey === key, onclick: () => {
    if (!armed) { state.ghArmed = key; clearTimeout(ghArmTimer); ghArmTimer = setTimeout(() => { state.ghArmed = ""; render(); }, 4000); render(); return; }
    state.ghArmed = ""; run();
  } }, state.ghBusyKey === key ? "Working…" : armed ? confirmLabel : label);
}

async function ghRemove(owner, forget, on) {
  const key = (forget ? "forget:" : "rm:") + owner;
  state.ghBusyKey = key; render();
  try {
    const res = await api("/api/github/remove", { method: "POST", headers: { "Content-Type": "application/json", "X-Offload-Dashboard": "1" }, body: JSON.stringify({ owner, forget, on }) });
    const bad = res.results.filter((x) => x.error);
    ghNotice(bad.length ? "critical" : "info", bad.length
      ? `Couldn't remove ${owner} from ${bad.map((x) => `${x.runner} (${x.error})`).join(", ")}`
      : forget ? `Removed ${owner} here. Delete its scale sets under Runner settings on GitHub too.` : `Removing ${owner}: its runners stop, then its scale sets are deleted on GitHub.`);
  } catch (e) {
    ghNotice("critical", `Couldn't remove ${owner}: ${e.message}`);
  } finally {
    state.ghBusyKey = ""; render();
  }
}
let ghNoticeTimer;
function ghNotice(kind, text) {
  state.ghNotice = { kind, text };
  clearTimeout(ghNoticeTimer);
  if (kind !== "critical") ghNoticeTimer = setTimeout(() => { state.ghNotice = null; render(); }, 10000);
}

// An owner's avatar (a data: URL from the runner) or, without one, its initial.
function ghAvatar(list) {
  const src = list.map(([, p]) => p.avatar).find((a) => /^data:image\/(png|jpeg|gif|webp);base64,[A-Za-z0-9+/=]+$/.test(a || ""));
  const owner = list[0][1].owner;
  return src ? h("img", { class: "gh-avatar", src, alt: "", width: 24, height: 24 })
    : h("span", { class: "gh-avatar initial", "aria-hidden": "true" }, owner.slice(0, 1).toUpperCase());
}

function ownerCard(owner, list) {
  const first = list[0][1];
  const display = list.map(([, p]) => p.name).find(Boolean);
  const key = (p) => ghLabels(p).map((l) => l.toLowerCase()).sort().join(",");
  const sameLabels = list.every(([, p]) => key(p) === key(first));
  const snippet = "runs-on: " + runsOn(first.labels);
  const sameCaches = list.every(([, p]) => !!p.shared === !!first.shared);
  const finished = sum(list, ([, p]) => p.runs);
  const last = list.map(([, p]) => p.last_run).filter(Boolean).sort().pop();
  const per = first.resources;
  const left = list.filter(([, p]) => p.state !== "removing").map(([r]) => r.status?.id).filter(Boolean);
  const stuckRows = list.filter(([, p]) => p.state === "removing" && (p.detail || "").startsWith("couldn't"));
  const stuck = stuckRows.map(([r]) => r.status?.id).filter(Boolean);
  return h("div", { class: "card gh-owner" },
    h("div", { class: "gh-owner-head" },
      h("h3", {}, ghAvatar(list), display || owner, display && display.toLowerCase() !== owner.toLowerCase() ? h("span", { class: "login" }, owner) : null),
      h("a", { class: "subtle-link", href: ghSettings(owner), target: "_blank", rel: "noopener noreferrer" }, "Runner settings", extIcon()),
      sameLabels ? h("div", { class: "runs-on" }, h("code", {}, snippet), copyBtn(snippet, snippet))
        : h("div", { class: "runs-on muted" }, "Runners match different labels")),
    h("div", { class: "gh-runners", role: "list" }, list.sort((a, b) => a[0].name.localeCompare(b[0].name)).map(([r, p]) =>
      h("div", { class: "gh-runner", role: "listitem" },
        h("span", { class: "n" }, r.name),
        poolChip(p),
        h("div", { class: "room" }, slots(p), h("span", { class: p.state === "failed" ? "crit-text" : "muted" }, roomText(p)),
          sameLabels ? null : h("span", { class: "runs-on-row" }, ...labelChips(ghLabels(p)), copyBtn("runs-on: " + runsOn(ghLabels(p)))),
          sameCaches ? null : h("span", { class: p.shared ? "" : "muted" }, p.shared ? "shared caches" : "clean caches"),
          null)))),
    sameLabels ? h("div", { class: "gh-also muted" }, "Labels ", ...labelChips(ghLabels(first)),
      h("span", {}, " · a workflow runs here when these include every label it asks for")) : null,
    h("div", { class: "runner-foot gh-foot" },
      h("span", {}, "Each job ", h("b", {}, `${fmtCores(per.cpu)}–${fmtCores(per.cpu_max)} cores`), " · ", h("b", {}, fmtBytes(per.mem))),
      h("span", {}, h("b", {}, finished), " finished", last ? " · last one " + agoCoarse(last) : ""),
      sameCaches ? h("span", {}, first.shared ? "Warm caches shared by its jobs" : "Clean caches every job") : h("span", {}, "Cache policy differs by runner"),
      h("span", {}, first.token_from === "gh" ? "With your gh login" : "With a token"),
      h("span", { class: "gh-actions" },
        stuck.length ? armedBtn("forget:" + owner, "Remove here only", `Confirm: GitHub keeps ${stuckRows.map(([r]) => r.name).join(", ")}'s scale set`, () => ghRemove(owner, true, stuck)) : null,
        left.length ? armedBtn("rm:" + owner, "Remove", "Confirm remove: stops its running jobs", () => ghRemove(owner, false, left)) : null)));
}

function ghOnboarding() {
  const add = "offload github add ORG";
  const use = "runs-on: offload";
  return h("div", { class: "card onboarding" },
    h("h2", {}, "Run your GitHub Actions jobs on these runners"),
    h("p", { class: "muted" }, "Each job gets a fresh container sized like any offload job, and GitHub sends it to whichever runner has room. Docker builds work."),
    h("ol", { class: "steps" },
      h("li", {}, h("div", { class: "what" }, "Connect an organization or repository. Offload uses your GitHub CLI login, or a token."),
        h("div", { class: "snippet" }, h("button", { type: "button", class: "btn primary", onclick: () => openGhForm() }, plusIcon(), "Add organization"),
          h("span", { class: "muted small-or" }, "or run"), h("code", {}, add), copyBtn(add))),
      h("li", {}, h("div", { class: "what" }, "Point workflows at it."),
        h("div", { class: "snippet" }, h("code", {}, use), copyBtn(use)))));
}

// ---- the add form
let ghForm;
function ghFormEl() {
  if (ghForm) return ghForm.root;
  const f = {};
  const field = (label, forId, control, hint) => h("div", { class: "gh-field" },
    h("label", { for: forId }, label), h("div", { class: "control" }, control, hint ? h("div", { class: "hint" }, hint) : null));
  const radio = (name, value, label, desc, checked) => {
    const input = h("input", { type: "radio", name, value, checked, onchange: () => renderGhForm() });
    return [input, h("label", { class: "choice" }, input, h("span", {}, h("b", {}, label), desc ? h("span", { class: "muted" }, desc) : null))];
  };
  f.owner = h("input", { type: "text", id: "gh-f-owner", placeholder: "ORG or ORG/REPO", autocomplete: "off", spellcheck: "false", "aria-describedby": "gh-f-owner-hint", oninput: () => renderGhForm() });
  f.labels = h("input", { type: "text", id: "gh-f-labels", value: "self-hosted, linux, offload", autocomplete: "off", spellcheck: "false", oninput: () => { f.labelsEdited = true; renderGhForm(); } });
  f.archNote = h("span", {});
  f.labelsNote = h("div", { class: "gh-form-result", hidden: true });
  f.preview = h("code", {});
  let c;
  [f.cacheClean, c] = radio("gh-cache", "clean", "Clean for every job", " Each job starts empty. Safe when the repositories don't all trust each other.", true);
  const cacheClean = c;
  [f.cacheShared, c] = radio("gh-cache", "shared", "Shared by its repositories", " Checkouts, dependency caches and docker images stay warm. One repository can affect what the next one runs.", false);
  f.cacheNote = h("div", { class: "hint" });
  f.cacheChoices = h("div", { class: "choices" }, cacheClean, c);
  [f.authGh, c] = radio("gh-auth", "gh", "Your GitHub CLI login", " From gh on this machine. Needs the admin:org scope for an organization.", true);
  const authGh = c;
  [f.authToken, c] = radio("gh-auth", "token", "A token", " A fine-grained token with only “Self-hosted runners: write” is the narrowest.", false);
  f.token = h("input", { type: "password", id: "gh-f-token", placeholder: "github_pat_…", autocomplete: "off", "aria-label": "Token" });
  f.tokenRow = h("div", { class: "token-row" }, f.token);
  f.runners = h("div", { class: "choices inline" });
  f.max = h("input", { type: "number", id: "gh-f-max", min: 0, step: 1, placeholder: "As many as fit", inputmode: "numeric" });
  f.result = h("div", { class: "gh-form-result", "aria-live": "polite" });
  f.exists = h("div", { class: "gh-form-result", hidden: true });
  f.submit = h("button", { type: "submit", class: "btn primary" }, "Add");
  f.root = h("form", { class: "card gh-form", hidden: true, novalidate: true, onsubmit: (e) => { e.preventDefault(); submitGhForm(); }, onkeydown: (e) => { if (e.key === "Escape") { e.stopPropagation(); closeGhForm(); } } },
    h("div", { class: "gh-form-head" }, h("h3", {}, "Add an organization or repository"),
      h("button", { type: "button", class: "icon-btn", "aria-label": "Close", onclick: closeGhForm }, s("svg", { viewBox: "0 0 16 16", width: 16, height: 16, "aria-hidden": "true" }, s("path", { d: "M4 4l8 8M12 4l-8 8", stroke: "currentColor", "stroke-width": 1.8, "stroke-linecap": "round" })))),
    field("Organization or repository", "gh-f-owner", f.owner, h("span", { id: "gh-f-owner-hint" }, "As on GitHub, like ", h("code", {}, "acme"), " or ", h("code", {}, "acme/app"), ".")),
    f.exists,
    field("Labels", "gh-f-labels", f.labels, ["Every label the runners get, comma-separated: GitHub's usual self-hosted ones and your own. ",
      "A workflow runs here when these include every label it asks for, like ", f.preview, ".", f.archNote]),
    f.labelsNote,
    h("fieldset", { class: "gh-field" }, h("legend", {}, "Caches"), h("div", { class: "control" }, f.cacheChoices, f.cacheNote)),
    h("fieldset", { class: "gh-field" }, h("legend", {}, "Connect with"), h("div", { class: "control" }, h("div", { class: "choices" }, authGh, c), f.tokenRow)),
    h("fieldset", { class: "gh-field" }, h("legend", {}, "Runners"), h("div", { class: "control" }, f.runners)),
    field("Most jobs at once", "gh-f-max", f.max, "Per runner. Empty takes as many as fit."),
    f.result,
    h("div", { class: "gh-form-actions" }, f.submit, h("button", { type: "button", class: "btn", onclick: closeGhForm }, "Cancel")));
  f.runnerKey = null;
  ghForm = f;
  return f.root;
}

function openGhForm() {
  state.ghFormOpen = true; state.ghFormMsg = null;
  if (ghForm) { ghForm.picks = null; ghForm.runnerKey = null; ghForm.lastOwner = null; ghForm.cacheClean.checked = true; ghForm.labelsEdited = false; }
  render();
  ghForm.root.scrollIntoView({ block: "nearest", behavior: "smooth" });
  ghForm.owner.focus();
}
function closeGhForm() {
  state.ghFormOpen = false; state.ghFormMsg = null;
  if (ghForm) ghForm.token.value = "";
  render();
  $("#gh-add-btn")?.focus();
}

function ghOwnerInput() {
  return ghForm.owner.value.trim().replace(/^https:\/\/(www\.)?github\.com\//i, "").replace(/\/+$/, "");
}

// The form only connects organizations that aren't yet: changing a connected one's token or
// settings is `offload github add` in a terminal (or remove it and add it again here).
function renderGhForm() {
  const f = ghForm;
  if (!f) return;
  f.root.hidden = !state.ghFormOpen;
  if (!state.ghFormOpen) return;
  const owners = state.ghOwners || new Map();
  const owner = ghOwnerInput();
  if (owner !== f.lastOwner) { // sharing is chosen for the name at hand, never carried over
    f.lastOwner = owner;
    f.cacheClean.checked = true;
  }
  const existing = [...owners.keys()].find((o) => o.toLowerCase() === owner.toLowerCase());
  const holders = new Set(existing ? owners.get(existing).map(([r]) => r.status?.id) : []); // runners that already have it
  const holderNames = existing ? owners.get(existing).map(([r]) => r.name) : [];
  const repo = owner.includes("/");
  f.cacheChoices.hidden = repo;
  const pools = existing ? owners.get(existing).map(([, p]) => p) : [];
  f.cacheNote.textContent = repo ? "A repository's jobs keep warm caches: they are all the same repository's code."
    : pools.length ? `${existing} has ${pools.some((p) => p.shared) ? "shared" : "clean"} caches on its other runners; these runners get what you choose here.` : "";
  // Rebuilt only when the owner's runners or their labels change, so a refresh never disturbs it.
  const sets = existing ? owners.get(existing).map(([r, p]) => [r.name, ghLabels(p)]) : [];
  const noteKey = (existing || "") + "|" + sets.map(([n, l]) => n + "=" + l.join(",")).join("|");
  if (noteKey !== f.labelsNoteKey) {
    f.labelsNoteKey = noteKey;
    f.labelsNote.hidden = !sets.length;
    f.labelsNote.replaceChildren(...(sets.length ? [ghAlert("info",
      h("div", {}, `${existing}'s runners have `, ...sets.map(([name, l], i) => [i ? "; " : "", ...labelChips(l), ` (${name})`]).flat(), "."))] : []));
  }
  f.exists.hidden = !existing;
  if (existing) {
    // Runners report these values, so the command is offered only when nothing in it needs quoting.
    const ids = owners.get(existing).map(([r]) => r.status?.id || "");
    const safe = /^[A-Za-z0-9][A-Za-z0-9._-]*(\/[A-Za-z0-9][A-Za-z0-9._-]*)?$/.test(existing) && ids.every((id) => /^[0-9a-f]{6,64}$/.test(id));
    const cmd = safe ? `offload github add ${existing} --on ${ids.join(",")}` : "";
    f.exists.replaceChildren(ghAlert("info", h("div", {}, `${existing} is already connected on ${holderNames.join(", ")}; you can add it to other runners here. `,
      "To give it a new token or change its settings, run offload github add with the new values and --on its runners, or remove it below and add it again."),
      cmd ? h("div", { class: "snippet" }, h("code", {}, cmd), copyBtn(cmd)) : null));
  }
  f.tokenRow.hidden = !f.authToken.checked;
  // Runners are chosen by their ID for as long as the form is open: a runner that drops out and
  // comes back keeps its choice, one that appears later starts unchosen, and a replacement under
  // the same name is a different runner.
  const runners = ghRunners();
  if (!f.picks && runners.length) f.picks = new Map(runners.map((r) => [r.status.id, true]));
  const key = runners.map((r) => r.status.id + "=" + r.name).join("\n") + "|" + [...holders].join(",");
  if (key !== f.runnerKey) {
    f.runnerKey = key;
    f.runners.replaceChildren(...(runners.length ? runners.map((r) => {
      const id = r.status.id;
      if (!f.picks.has(id)) f.picks.set(id, false);
      const has = holders.has(id);
      const input = h("input", { type: "checkbox", value: id, checked: !has && f.picks.get(id), disabled: has, onchange: (e) => { f.picks.set(id, e.target.checked); renderGhForm(); } });
      return h("label", { class: "choice" + (has ? " off" : "") }, input, h("span", {}, h("b", {}, r.name),
        h("span", { class: "muted" }, has ? " already connected" : ` ${fmtCores(r.status.capacity.cpu)} cores`)));
    }) : [h("span", { class: "muted" }, "No runner can take GitHub jobs: they need podman, and must be up to date.")]));
  }
  // Until the box is edited it follows the choice: the labels the owner's runners already share,
  // else GitHub's usual ones for the chosen runners and offload.
  const chosen = runners.filter((r) => f.picks?.get(r.status.id) && !holders.has(r.status.id));
  if (!f.labelsEdited) {
    const keys = sets.map(([, l]) => l.map((x) => x.toLowerCase()).sort().join(","));
    const want = (sets.length && keys.every((k) => k === keys[0]) ? ghDefaultLabels(chosen, sets[0][1]) : ghDefaultLabels(chosen)).join(", ");
    if (f.labels.value !== want) f.labels.value = want;
  }
  const boxLabels = f.labels.value.split(",").map((x) => x.trim()).filter(Boolean);
  f.preview.textContent = boxLabels.length ? "runs-on: " + runsOn(boxLabels) : "runs-on: …";
  const archs = [...new Set(chosen.map(ghArch).filter(Boolean))];
  f.archNote.textContent = archs.length > 1 ? ` The chosen runners differ in architecture (${archs.join(", ")}): list one only if every chosen runner has it.` : "";
  f.submit.disabled = !!state.ghFormBusy || (existing && ghRunners().every((r) => holders.has(r.status.id)));
  f.submit.textContent = state.ghFormBusy ? "Adding…" : "Add";
  const m = state.ghFormMsg;
  f.result.replaceChildren(...(m ? ghFormMessage(m) : []));
}

function ghFormMessage(m) {
  const lines = [];
  const text = (t) => {
    const cmd = (t.match(/gh auth refresh[^\n]*/) || [])[0];
    return [h("div", { class: "msg" }, t.replace(/\n\n\s*gh auth refresh[^\n]*\n\n/, " ").trim()), cmd ? h("div", { class: "snippet" }, h("code", {}, cmd), copyBtn(cmd)) : null];
  };
  if (m.error) lines.push(ghAlert("critical", text(m.error)));
  for (const r of m.results || []) {
    lines.push(r.error ? ghAlert("critical", h("b", {}, r.runner), ": ", text(r.error)) : ghAlert("good", h("b", {}, r.runner), ": connecting to GitHub"));
  }
  return lines;
}

async function submitGhForm() {
  const f = ghForm;
  if (state.ghFormBusy) return;
  const owner = ghOwnerInput();
  const fail = (error, el) => { state.ghFormMsg = { error }; render(); el?.focus(); };
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]*(\/[A-Za-z0-9][A-Za-z0-9._-]*)?$/.test(owner)) return fail("Enter an organization (acme) or a repository (acme/app).", f.owner);
  const on = [...f.runners.querySelectorAll("input:checked:not(:disabled)")].map((i) => i.value); // runner IDs
  if (!on.length) return fail("Pick at least one runner.", f.runners.querySelector("input"));
  const token = f.authToken.checked ? f.token.value.trim() : "";
  if (f.authToken.checked && !token) return fail("Paste the token, or connect with your GitHub CLI login.", f.token);
  const max = f.max.value === "" ? 0 : Number(f.max.value);
  if (!Number.isInteger(max) || max < 0) return fail("Most jobs at once is a whole number, or empty.", f.max);
  // What the box shows is exactly what the runners get: no defaults are added behind it.
  const body = { owner, token, on, max, labels: f.labels.value.split(",").map((x) => x.trim()).filter(Boolean), system_labels: false };
  if (!body.labels.length) return fail("Give the runners at least one label.", f.labels);
  if (!owner.includes("/")) body.shared = f.cacheShared.checked;
  state.ghFormBusy = true; state.ghFormMsg = null; render();
  try {
    const res = await api("/api/github/add", { method: "POST", headers: { "Content-Type": "application/json", "X-Offload-Dashboard": "1" }, body: JSON.stringify(body) });
    const bad = res.results.filter((x) => x.error);
    if (!bad.length) {
      f.owner.value = ""; f.token.value = "";
      state.ghFormOpen = false;
      ghNotice("good", `Connected ${owner} on ${res.results.map((x) => x.runner).join(", ")}. Each runner shows Preparing until it is ready; workflows use ${"runs-on: " + runsOn(body.labels)}.`);
    } else {
      state.ghFormMsg = { results: res.results };
    }
  } catch (e) {
    state.ghFormMsg = { error: e.message };
  } finally {
    state.ghFormBusy = false;
    render();
  }
}

function ghDrawerHead(w) {
  const g = w.github;
  return [
    h("div", { class: "row" }, h("span", { class: "kind" }, "GitHub"), h("h2", {}, g.job || (isActive(w) ? "Waiting for a job" : "Took no job")),
      ghStatus(w), live(w) ? stopBtn(w) : null, closeBtn()),
    h("div", { class: "gh-head-sub" },
      h("span", {}, g.repo || g.owner, g.workflow ? h("span", { class: "muted" }, " · " + g.workflow) : null, h("span", { class: "muted mono" }, " · " + w.id)),
      g.url ? h("a", { class: "btn", href: g.url, target: "_blank", rel: "noopener noreferrer" }, "Open on GitHub", extIcon()) : null),
  ];
}

// ---------- drawer ----------
let scrim;
let logView = null; // {key, stream, abort, pre, follow, status}
function openDrawer() {
  const d = $("#drawer");
  if (!scrim) { scrim = h("div", { class: "scrim", onclick: closeDrawer }); document.body.append(scrim); }
  const open = !!state.sel;
  d.classList.toggle("open", open);
  scrim.classList.toggle("open", open);
  d.setAttribute("aria-hidden", String(!open));
  if (!open) { stopLogs(); d.replaceChildren(); return; }
  d.replaceChildren();
  renderDrawer();
}
function closeDrawer() { location.hash = state.view === "github" ? "#/github" : "#/containers"; }
addEventListener("keydown", (e) => { if (e.key === "Escape" && state.sel) closeDrawer(); });

function findWorkload() {
  return workloads().find((w) => w.runner === state.sel.runner && w.id === state.sel.id);
}

function renderDrawer() {
  const d = $("#drawer");
  const w = state.fleet && findWorkload();
  if (!w) {
    d.replaceChildren(h("div", { class: "drawer-head" }, h("div", { class: "row" }, h("h2", {}, state.sel.id), closeBtn())),
      h("div", { class: "drawer-body" }, h("div", { class: "empty-state" }, state.fleet ? "This workload is no longer listed by its runner (finished more than an hour ago, or collected)." : "Loading…")));
    stopLogs();
    return;
  }
  let head = d.querySelector(".drawer-head"), body = d.querySelector(".drawer-body");
  if (!d.querySelector("#dr-info")) { // first render, or replacing the loading placeholder
    head = h("div", { class: "drawer-head" }); body = h("div", { class: "drawer-body" });
    body.append(h("div", { id: "dr-info" }), h("div", { id: "dr-logs" }));
    d.replaceChildren(head, body);
  }
  head.replaceChildren(...(w.github ? ghDrawerHead(w) : [
    h("div", { class: "row" }, h("span", { class: "kind" }, w.kind === "job" ? "Job" : "Session"), h("h2", { class: "mono" }, w.id), exitChip(w),
      live(w) ? stopBtn(w) : null, closeBtn()),
    h("div", { class: "cmdline mono" }, w.kind === "job" ? w.cmd : "profile " + w.cmd)]));
  const err = state.stopping.get(w.runner + "/" + w.id);
  $("#dr-info").replaceChildren(err ? h("div", { class: "alert critical", role: "alert", style: { marginBottom: "14px" } }, icon("critical"), h("div", {}, err)) : "", ...drawerInfo(w));
  ensureLogs(w);
}
function closeBtn() {
  return h("button", { class: "icon-btn", "aria-label": "Close", onclick: closeDrawer },
    s("svg", { viewBox: "0 0 16 16", width: 16, height: 16, "aria-hidden": "true" }, s("path", { d: "M4 4l8 8M12 4l-8 8", stroke: "currentColor", "stroke-width": 1.8, "stroke-linecap": "round" })));
}
let confirmFor = null, confirmTimer;
function stopBtn(w) {
  const key = w.runner + "/" + w.id;
  if (state.stopping.get(key) === "") return h("button", { class: "btn danger", disabled: true, style: { marginLeft: "auto" } }, "Stopping…");
  const confirming = confirmFor === key;
  const label = w.github ? ["Stop runner", "Confirm: fails the GitHub job"] : w.kind === "job" ? ["Cancel job", "Confirm cancel"] : ["Stop session", "Confirm stop"];
  const b = h("button", { class: "btn danger" + (confirming ? " confirm" : ""), style: { marginLeft: "auto" } }, confirming ? label[1] : label[0]);
  b.addEventListener("click", async () => {
    if (!confirming) { confirmFor = key; clearTimeout(confirmTimer); confirmTimer = setTimeout(() => { confirmFor = null; renderDrawer(); }, 4000); renderDrawer(); return; }
    confirmFor = null; state.stopping.set(key, ""); renderDrawer();
    try {
      await api("/api/stop", { method: "POST", headers: { "Content-Type": "application/json", "X-Offload-Dashboard": "1" }, body: JSON.stringify({ host: w.runner, kind: w.kind, id: w.id }) });
    } catch (e) { state.stopping.set(key, "Couldn't stop it: " + e.message); renderDrawer(); }
  });
  return b;
}

function sparkline(points, key, color, fmt, max) {
  const W = 260, H = 36;
  const svg = s("svg", { viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", "aria-hidden": "true" });
  if (points.length < 2) return svg;
  const t0 = points[0].t, t1 = points[points.length - 1].t;
  const ym = Math.max(max || 0, ...points.map((p) => p[key])) || 1;
  const d = points.map((p, i) => (i ? "L" : "M") + (((p.t - t0) / Math.max(1, t1 - t0)) * W).toFixed(1) + "," + (H - 3 - (p[key] / ym) * (H - 6)).toFixed(1)).join("");
  svg.append(s("path", { d, fill: "none", "stroke-width": 2, "vector-effect": "non-scaling-stroke", "stroke-linejoin": "round", style: { stroke: `var(${color})` } }));
  return svg;
}

function drawerInfo(w) {
  const r = w.raw, res = w.res || {};
  const out = [];
  if (live(w) && w.state === "running") {
    const hist = state.usage.get(w.runner + "/" + w.id) || [];
    out.push(h("div", { class: "sparks" },
      h("div", { class: "spark" }, h("div", { class: "k" }, "CPU now"), h("div", { class: "v num" }, w.use ? fmtCores(w.use.cpu) + (fmtCores(w.use.cpu) === "1" ? " core" : " cores") : "–", h("span", { class: "muted", style: { fontSize: "12px", fontWeight: 400 } }, `  of ${fmtCores(res.cpu)} guaranteed, ${fmtCores(res.cpu_max)} max`)),
        sparkline(hist, "cpu", "--series-2", fmtCores, res.cpu_max)),
      h("div", { class: "spark" }, h("div", { class: "k" }, "Memory now"), h("div", { class: "v num" }, w.use ? fmtBytes(w.use.mem) : "–", h("span", { class: "muted", style: { fontSize: "12px", fontWeight: 400 } }, `  of ${fmtBytes(res.mem)}${w.use?.mem_peak ? ", peak " + fmtBytes(w.use.mem_peak) : ""}`)),
        sparkline(hist, "mem", "--series-2", fmtBytes, res.mem))));
  }
  const rows = [];
  const add = (k, v) => { if (v != null && v !== "" && v !== false) rows.push(h("dt", {}, k), h("dd", {}, v)); };
  add("Runner", stale(w) ? `${w.runner} (not answering${w.host.last_seen ? ", last seen " + ago(w.host.last_seen) : ""}: this is its last report)` : w.runner);
  if (r.reason) add("Reason", r.reason.replace(/^waiting: /, "Waiting: "));
  if (w.github) {
    const g = w.github;
    add("Repository", g.repo ? h("a", { href: "https://github.com/" + g.repo, target: "_blank", rel: "noopener noreferrer" }, g.repo) : null);
    add("Workflow", g.workflow ? h("span", { class: "mono" }, g.workflow) : null);
    add("Triggered by", g.event ? g.event.replace(/_/g, " ") : null);
    add("GitHub's result", g.result ? g.result.replace(/^canceled$/, "cancelled") : null);
    add("Runner on GitHub", h("span", { class: "mono" }, g.runner || "–"));
    add("For", g.owner ? h("span", {}, g.owner, h("span", { class: "muted" }, " · " + (ghPool(w.host, g.owner)?.scale_set || ""))) : null);
  } else if (w.kind === "job") {
    const sp = r.spec;
    add("Profile", sp.profile);
    add("Repository", sp.workspace_key ? h("span", { class: "mono" }, sp.workspace_key) : "none (throwaway directory)");
    add("Directory", sp.dir && sp.dir !== "." ? h("span", { class: "mono" }, sp.dir) : null);
    add("Git", sp.git ? h("span", { class: "mono" }, (sp.git.branch ? sp.git.branch + " @ " : "") + sp.git.head.slice(0, 12)) : null);
    const up = r.uploaded; // absent until the sync finished (or from an older runner)
    add("Synced files", !r.sync_files ? null : up == null ? `${r.sync_files}` :
      `${r.sync_files} · ` + (up === 0 ? "all already on the runner" : up === r.sync_files ? "all uploaded" : `${up} uploaded, ${r.sync_files - up} already on the runner`));
  } else {
    add("Profile", r.profile);
    add("Clients attached", String(r.attached));
    add("Last client contact", r.last_seen ? ago(r.last_seen) : null);
    add("Network", r.network === "client" ? "through the attached client" : null);
  }
  if (!w.github) add("Caller", [w.raw.caller, w.kind === "job" ? r.spec.client : r.client].filter(Boolean).filter((v, i, a) => a.indexOf(v) === i).join(" · "));
  add("Runtime", w.runtime === "container" ? h("span", {}, "container ", h("span", { class: "mono muted" }, w.image || "")) : w.runtime);
  add("Resources", `${fmtCores(res.cpu)} ${fmtCores(res.cpu) === "1" ? "core" : "cores"} guaranteed, up to ${fmtCores(res.cpu_max)} · ${fmtBytes(res.mem)} memory` + (r.mem_from ? ` (${r.mem_from === "learned" ? "learned from earlier runs" : r.mem_from})` : ""));
  add("Pinned CPUs", w.cpus ? h("span", { class: "mono" }, w.cpus) : null);
  add("Created", fmtTime(w.created) + " (" + ago(w.created) + ")");
  if (w.kind === "job" && r.started_at) add("Waited", fmtDur((new Date(r.started_at) - new Date(r.created_at)) / 1000));
  if (w.finished && w.started) add("Ran for", fmtDur((new Date(w.finished) - new Date(w.started)) / 1000));
  else if (w.started && live(w) && w.state === "running") add("Running for", fmtDur(since(w.started)));
  if (r.exit_code != null) add("Exit code", String(r.exit_code) + (r.signal ? " (" + r.signal + ")" : ""));
  if (r.peak_mem) add("Peak memory", fmtBytes(r.peak_mem) + (res.mem ? ` (${pct(r.peak_mem / res.mem)} of the limit)` : ""));
  if (r.cpu_seconds) add("CPU time", fmtDur(r.cpu_seconds));
  if (w.kind === "job") {
    if (!w.github) add("Secrets", (r.spec.secret_names || []).length ? r.spec.secret_names.join(", ") : null);
    add("Write-back", r.write_back ? `${(r.write_back.modified || []).length} file(s) changed` : null);
    add("Network", r.spec.network === "client" ? "through the client" : null);
  }
  out.push(h("dl", { class: "details" }, rows));
  return out;
}

// Logs: one streaming request at a time, restarted when the stream tab changes.
function ensureLogs(w) {
  const box = $("#dr-logs");
  const key = w.runner + "/" + w.id;
  const streams = w.kind === "job" ? [["stdout", "Output"], ["stderr", "Errors and offload notes"]] : [["stderr", "Log"], ["stdout", "MCP protocol (stdout)"]];
  if (logView && logView.key === key) { logView.w = w; return; }
  stopLogs();
  logView = { key, w, stream: streams[0][0], follow: true };
  const pre = h("pre", { class: "log", tabindex: 0, "aria-label": "Log output" });
  pre.addEventListener("scroll", () => { logView && (logView.follow = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 24); });
  const status = h("span", { class: "muted" });
  const tabs = () => segmented(streams, logView.stream, (v) => { logView.stream = v; head.replaceChild(tabs(), head.firstChild); startLogs(); }, "Log stream");
  const head = h("div", { class: "logs-head" }, tabs(), status);
  box.replaceChildren(h("div", { class: "section-head" }, h("h2", {}, "Logs")), head, pre);
  logView.pre = pre; logView.status = status;
  startLogs();
}

function stopLogs() {
  if (logView?.abort) logView.abort.abort();
  logView = null;
}

async function startLogs() {
  const v = logView;
  if (!v) return;
  if (v.abort) v.abort.abort();
  const ac = new AbortController();
  v.abort = ac;
  v.pre.replaceChildren();
  v.status.textContent = "Connecting…";
  const tail = 256 * 1024;
  const url = `/api/logs?host=${encodeURIComponent(v.w.runner)}&kind=${v.w.kind}&id=${encodeURIComponent(v.w.id)}&stream=${v.stream}&tail=${tail}`;
  try {
    const r = await fetch(url, withKey({ signal: ac.signal }));
    if (!r.ok) {
      let msg = r.statusText;
      try { msg = (await r.json()).error || msg; } catch {}
      v.status.textContent = msg === "no output yet" ? "No output yet" : msg;
      if (msg === "no output yet" && isActive(v.w)) setTimeout(() => logView === v && v.abort === ac && startLogs(), 3000);
      return;
    }
    v.status.textContent = isActive(v.w) ? "Streaming" : "Complete";
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    let size = 0, preamble = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      let text = dec.decode(value, { stream: true });
      if (preamble !== null) { // the first line is {"offset":N}
        preamble += text;
        const nl = preamble.indexOf("\n");
        if (nl < 0) continue;
        const off = JSON.parse(preamble.slice(0, nl)).offset || 0;
        if (off > 0) v.pre.append(h("span", { class: "gap" }, `… ${fmtBytes(off)} of earlier output not shown\n`));
        text = preamble.slice(nl + 1); // the runner starts a tail at a line boundary
        preamble = null;
      }
      text = stripAnsi(text);
      size += text.length;
      v.pre.append(document.createTextNode(text));
      while (size > 2 * 1024 * 1024 && v.pre.childNodes.length > 1) { size -= v.pre.firstChild.textContent.length; v.pre.firstChild.remove(); }
      if (v.follow) v.pre.scrollTop = v.pre.scrollHeight;
    }
    if (logView === v && v.abort === ac) v.status.textContent = "Ended";
  } catch (e) {
    if (e.name !== "AbortError" && logView === v) v.status.textContent = "Disconnected: " + e.message;
  }
}

// ---------- devices ----------
// Phones paired with this dashboard reach its API over the tailnet with a token of their own.
// Pairing shows a one-time code (good for ten minutes) to type into the phone.
state.devices = null; state.devErr = null; state.devBusy = ""; state.devArmed = "";
let devTimer, devArmTimer;
async function loadDevices() {
  clearTimeout(devTimer);
  try { state.devices = await api("/api/devices"); state.devErr = null; } catch (e) { state.devErr = e.message; }
  if (state.view === "devices") { renderDevices(); devTimer = setTimeout(loadDevices, state.devices?.code ? 2000 : 10000); }
}
async function devPost(path, body) {
  return api(path, { method: "POST", headers: { "Content-Type": "application/json", "X-Offload-Dashboard": "1" }, body: JSON.stringify(body || {}) });
}
function renderDevices() {
  const main = $("#main");
  mainBuilt = "devices";
  const d = state.devices;
  if (!d) { main.replaceChildren(h("div", { class: "card empty-state" }, state.devErr || "Loading…")); return; }
  const reach = d.addresses?.length ? d.addresses : null;
  const host = d.name || (reach ? reach[0].replace(/:\d+$/, "") : "");
  const port = reach ? reach[0].replace(/^.*:/, "") : "";
  const kids = [];
  if (state.devErr) kids.push(h("div", { class: "alerts" }, h("div", { class: "alert critical", role: "alert" }, icon("critical"), h("div", {}, state.devErr))));
  if (d.error) kids.push(h("div", { class: "alerts" }, h("div", { class: "alert warning" }, icon("warning"), h("div", {}, d.error))));
  let pair;
  if (d.code) {
    const left = Math.max(0, (new Date(d.code_expires) - Date.now()) / 1000);
    pair = h("div", { class: "card pair-card" },
      h("div", { class: "pair-steps" },
        h("h3", {}, "Pair a phone"),
        h("ol", {},
          h("li", {}, "Install the offload app on the phone and make sure Tailscale is on there."),
          h("li", {}, "In the app, enter this computer's address and the code."),
          h("li", {}, "The phone keeps its own key: it stays paired across restarts until you forget it here.")),
        h("div", { class: "pair-actions" }, h("button", { type: "button", class: "btn", onclick: async () => { await devPost("/api/devices/cancel"); loadDevices(); } }, "Cancel"))),
      h("dl", { class: "pair-values" },
        h("dt", {}, "Address"), h("dd", { class: "mono big" }, host + (port && port !== "7421" ? ":" + port : "")),
        d.dns && d.dns !== host ? [h("dt", {}, "Or"), h("dd", { class: "mono" }, d.dns + (port && port !== "7421" ? ":" + port : ""))] : null,
        reach ? [h("dt", {}, "Or by IP"), h("dd", { class: "mono" }, reach.join("  "))] : null,
        h("dt", {}, "Code"), h("dd", { class: "mono code" }, d.code.slice(0, 4) + "-" + d.code.slice(4)),
        h("dt", {}, "Expires"), h("dd", { class: "muted" }, "in " + fmtDur(left))));
  } else {
    pair = h("div", { class: "card pair-card" },
      h("div", { class: "pair-steps" },
        h("h3", {}, "Pair a phone"),
        h("p", { class: "muted" }, "The offload app shows everything this dashboard does: runners, containers, logs, GitHub jobs and activity, and it can pause runners and cancel work."),
        h("div", { class: "pair-actions" }, h("button", { type: "button", class: "btn primary", disabled: !reach || state.devBusy === "pair", onclick: async () => {
          state.devBusy = "pair"; renderDevices();
          try { await devPost("/api/devices/pair"); } catch (e) { state.devErr = e.message; }
          state.devBusy = ""; loadDevices();
        } }, plusIcon(), "Pair a phone"))));
  }
  kids.push(h("section", {}, pair));
  const list = d.devices || [];
  kids.push(h("section", {},
    h("div", { class: "section-head" }, h("h2", {}, "Paired devices"), h("span", { class: "muted" }, list.length ? "Each has its own key. Forget one to cut it off at once." : "")),
    list.length ? h("div", { class: "card device-list" }, list.map((x) => {
      const armed = state.devArmed === x.id;
      const seen = x.last_seen ? (since(x.last_seen) < 15 ? "Connected now" : "Last seen " + agoCoarse(x.last_seen)) : "Not seen since the dashboard started";
      return h("div", { class: "device-row" },
        h("div", {}, h("div", { class: "name" }, x.name), h("div", { class: "muted" }, "Paired " + new Date(x.paired_at).toLocaleDateString([], { month: "short", day: "numeric", year: "numeric" }) + " · " + seen)),
        h("button", { type: "button", class: "btn danger" + (armed ? " confirm" : ""), disabled: state.devBusy === x.id, onclick: async () => {
          if (!armed) { state.devArmed = x.id; clearTimeout(devArmTimer); devArmTimer = setTimeout(() => { state.devArmed = ""; renderDevices(); }, 4000); renderDevices(); return; }
          state.devArmed = ""; state.devBusy = x.id; renderDevices();
          try { await devPost("/api/devices/forget", { id: x.id }); } catch (e) { state.devErr = e.message; }
          state.devBusy = ""; loadDevices();
        } }, state.devBusy === x.id ? "Forgetting…" : armed ? "Forget " + x.name + "?" : "Forget"));
    })) : h("div", { class: "card empty-state" }, "No phones yet.")));
  main.replaceChildren(...kids);
}

// ---------- theme ----------
// System, light or dark. Colours are CSS variables, so switching only flips data-theme on the
// root; theme.js applies the saved choice before the first paint.
const glyphSVG = (...kids) => s("svg", { viewBox: "0 0 16 16", fill: "none", stroke: "currentColor", "stroke-width": "1.5", "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true" }, kids);
const THEMES = [
  ["system", "Match system", () => glyphSVG(s("rect", { x: "1.75", y: "2.75", width: "12.5", height: "8.5", rx: "1.5" }), s("path", { d: "M5.5 14h5M8 11.25V14" }))],
  ["light", "Light", () => glyphSVG(s("circle", { cx: "8", cy: "8", r: "2.75" }), s("path", { d: "M8 1.5v1.5M8 13v1.5M1.5 8H3M13 8h1.5M3.4 3.4l1.06 1.06M11.54 11.54l1.06 1.06M3.4 12.6l1.06-1.06M11.54 4.46l1.06-1.06" }))],
  ["dark", "Dark", () => glyphSVG(s("path", { d: "M13.5 9.6A5.75 5.75 0 0 1 6.4 2.5a5.75 5.75 0 1 0 7.1 7.1Z" }))],
];
const narrow = matchMedia("(max-width: 760px)");
let theme = "system"; // in memory too: storage may be blocked
function currentTheme() { const t = store("theme"); return t === "light" || t === "dark" || t === "system" ? t : theme; }
function setTheme(t) { theme = t; store("theme", t); applyTheme(t); }
function applyTheme(t = currentTheme()) {
  theme = t;
  if (t === "system") delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = t;
  renderTheme();
}
function renderTheme() {
  const cur = theme;
  $("#theme").replaceChildren(h("div", { class: "seg theme-switch", role: "group", "aria-label": "Theme" },
    THEMES.map(([v, label, glyph], i) => h("button", { type: "button", "aria-pressed": String(v === cur), "aria-label": label, title: label, onclick: () => {
      // On a phone only the current choice shows (see app.css): tapping it moves to the next.
      setTheme(v === cur && narrow.matches ? THEMES[(i + 1) % THEMES.length][0] : v);
    } }, glyph()))));
}
// Another dashboard tab changed it.
addEventListener("storage", (e) => { if (e.key === "theme") applyTheme(); });

// ---------- start ----------
window.addEventListener("resize", () => { if (state.view === "overview") renderCharts(); });
applyTheme();
route();
pollFleet();
