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
  }[kind] || [];
  return s("svg", { viewBox: "0 0 16 16", "aria-hidden": "true" }, p);
}
function chip(kind, label) {
  const cls = kind === "running" ? "info" : kind;
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
  sel: null, // {runner, id}
  stopping: new Map(), // "runner/id" -> "" while stopping, or the error
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

const metricRunners = () => (state.fleet?.runners || []).filter((r) => r.online && r.status?.metrics && (!state.runner || r.name === state.runner));

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
  const view = parts[0] === "containers" ? "containers" : "overview";
  const changed = view !== state.view;
  state.view = view;
  const sel = view === "containers" && parts[1] && parts[2] ? { runner: parts[1], id: parts[2] } : null;
  if (JSON.stringify(sel) !== JSON.stringify(state.sel)) { state.sel = sel; openDrawer(); }
  for (const a of document.querySelectorAll(".tabs a")) {
    if (a.dataset.view === view) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  }
  if (changed) { mainBuilt = ""; if (view === "overview") scheduleData(); }
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
    if (!st.accepting) out.push(["warning", [h("b", {}, r.name), " is not accepting work: ", st.reason || "unknown reason"]]);
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
  if (state.view === "overview") renderOverview(); else renderContainers();
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
  $("#ov-runners").replaceChildren(...runners.map(runnerCard));
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
    kpi("Memory free", fmtBytes(t.freeMem), `of ${fmtBytes(t.capMem)} not yet allocated · hosts have ${fmtBytes(t.memAvail)} available`),
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
  if (j) return id + " · " + truncate(cmdline(j.spec.argv), 60);
  const se = r.sessions.find((x) => x.id === id);
  return se ? id + " · session " + se.profile : id;
}
const truncate = (t, n) => (t.length > n ? t.slice(0, n - 1) + "…" : t);

function runnerCard(r) {
  const st = r.status;
  const head = h("div", { class: "runner-head" }, h("h3", {}, r.name),
    !r.online ? chip("critical", "Offline") : st && !st.accepting ? chip("warning", "Not accepting") : chip("good", "Accepting"));
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
        cell.addEventListener("click", () => { if (c.owners?.length) location.hash = `#/containers/${encodeURIComponent(r.name)}/${encodeURIComponent(c.owners[0])}`; });
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
      h("span", { class: "v num" }, `${fmtBytes(st.allocated.mem)} of ${fmtBytes(st.capacity.mem)} allocated · host ${fmtBytes(st.mem_available)} available, reserve ${fmtBytes(st.mem_reserve)}`)),
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

function renderQueue(runners) {
  const el = $("#ov-queue");
  const waiting = [];
  for (const r of runners) for (const j of r.jobs) if (j.state === "queued" || j.state === "uploading") waiting.push([r, j]);
  if (!waiting.length) { el.replaceChildren(); return; }
  waiting.sort((a, b) => new Date(a[1].created_at) - new Date(b[1].created_at));
  el.replaceChildren(
    h("div", { class: "section-head" }, h("h2", {}, "Waiting"), h("span", { class: "muted" }, `${waiting.length} job${waiting.length === 1 ? "" : "s"} not started yet`)),
    h("div", { class: "table-wrap" }, h("table", {},
      h("thead", {}, h("tr", {}, ["Job", "Command", "Runner", "Waiting", "Asks for", "Why"].map((t) => h("th", {}, t)))),
      h("tbody", {}, waiting.map(([r, j]) => h("tr", { class: "click", tabindex: 0, onclick: () => (location.hash = `#/containers/${encodeURIComponent(r.name)}/${j.id}`) },
        h("td", { class: "mono" }, j.id), h("td", {}, h("span", { class: "cmd mono" }, cmdline(j.spec.argv))), h("td", {}, r.name),
        h("td", { class: "num nowrap" }, fmtDur(since(j.created_at))),
        h("td", { class: "num nowrap" }, `${fmtCores(j.resources.cpu)} cores · ${fmtBytes(j.resources.mem)}`),
        h("td", { class: "hint" }, (j.reason || (j.state === "uploading" ? "receiving source files" : "")).replace(/^waiting: /, ""))))))));
}

// ---------- charts ----------
const SERIES = {
  cpu: [["cpu_busy", "Whole host busy", "--series-1"], ["cpu_used", "Used by offload", "--series-2"], ["cpu_alloc", "Guaranteed to work", "--series-3"]],
  mem: [["mem_used", "Whole host in use", "--series-1"], ["mem_work", "Used by offload", "--series-2"], ["mem_alloc", "Allocated limits", "--series-3"]],
};

// combine sums every runner's series into one, aligned on step buckets.
function combine(seriesByRunner) {
  const all = Object.values(seriesByRunner).filter((x) => x && x.points.length);
  if (!all.length) return null;
  const step = Math.max(...all.map((x) => x.step));
  const buckets = new Map();
  for (const sr of all) {
    const per = new Map();
    for (const p of sr.points) {
      const b = Math.floor(p.t / step) * step;
      const acc = per.get(b) || { n: 0 };
      for (const k of Object.keys(p)) if (k !== "t") acc[k] = (acc[k] || 0) + p[k];
      acc.n++;
      per.set(b, acc);
    }
    for (const [b, acc] of per) {
      const tot = buckets.get(b) || { t: b };
      for (const k of Object.keys(acc)) if (k !== "n") tot[k] = (tot[k] || 0) + acc[k] / acc.n;
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
  const data = combine(state.series);
  const capCPU = sum(st, (x) => x.capacity.cpu), hostCPUs = sum(st, (x) => x.host_cpus);
  const capMem = sum(st, (x) => x.capacity.mem), memTotal = sum(runners, (r) => r.metrics?.mem_total);
  const sub = $("#ov-charts-sub");
  if (sub) sub.textContent = state.runner ? state.runner : runners.length > 1 ? `All ${runners.length} runners, added together` : "";
  const loading = state.seriesLoading && state.seriesFor !== state.range + "|" + state.runner;
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
  requestAnimationFrame(() => drawChart(box, data, series, o));
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
    svg.append(s("text", { x: W / 2, y: H / 2, "text-anchor": "middle", class: "empty" }, data ? "Collecting samples…" : "No usage data yet"));
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
  const warm = jobs.filter((e) => e.sync_files > 0);
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
      kind: "job", runner: r.name, id: j.id, state: j.state, reason: j.reason, exit_code: j.exit_code, cmd: cmdline(j.spec.argv),
      repo: shortKey(j.spec.workspace_key), caller: j.caller || j.spec.client || "", res: j.resources, cpus: j.allowed_cpus,
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
    segmented([["", "Jobs and sessions"], ["job", "Jobs"], ["session", "Sessions"]], f.kind, (v) => { f.kind = v; again(); }, "Kind"),
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
  let list = workloads().filter((w) =>
    (f.state === "all" || (f.state === "active") === isActive(w)) && (!f.kind || w.kind === f.kind) && (!f.runner || w.runner === f.runner) &&
    (!q || [w.id, w.cmd, w.repo, w.caller, w.runner, w.state].some((x) => (x || "").toLowerCase().includes(q))));
  list.sort((a, b) => (isActive(b) - isActive(a)) || new Date(b.created) - new Date(a.created));
  if (!list.length) {
    const msg = !state.fleet?.runners.length ? "No runners found yet." : f.state === "active" ? "Nothing is running. Jobs you start with offload run show up here." : "No workloads match.";
    el.replaceChildren(h("div", { class: "card empty-state" }, msg));
    return;
  }
  const sel = state.sel;
  const showRunner = (state.fleet?.runners || []).length > 1, showCPUs = list.some((w) => w.cpus);
  el.replaceChildren(h("div", { class: "table-wrap" }, h("table", {},
    h("thead", {}, h("tr", {}, ["Workload", "Command", showRunner && "Runner", "State", "CPU used (cores)", "Memory", showCPUs && "Pinned CPUs", "Repository", "Caller", "Time"].filter(Boolean).map((t) => h("th", {}, t)))),
    h("tbody", {}, list.map((w) => {
      const open = () => (location.hash = `#/containers/${encodeURIComponent(w.runner)}/${w.id}`);
      const t = w.finished ? `ended ${ago(w.finished)}` : w.started && w.state === "running" ? `for ${fmtDur(since(w.started))}` : `waiting ${fmtDur(since(w.created))}`;
      return h("tr", { class: "click" + (sel && sel.id === w.id && sel.runner === w.runner ? " sel" : ""), tabindex: 0, onclick: open, onkeydown: (e) => { if (e.key === "Enter") open(); } },
        h("td", { class: "nowrap" }, h("span", { class: "kind" }, w.kind === "job" ? "Job" : "Session"), h("span", { class: "mono" }, w.id)),
        h("td", {}, h("span", { class: "cmd mono", title: w.cmd }, w.cmd)),
        showRunner && h("td", { class: "nowrap" }, w.runner),
        h("td", {}, exitChip(w)),
        h("td", {}, isActive(w) ? usageCell(w.use?.cpu, w.res.cpu, fmtCores, " guaranteed") : h("span", { class: "muted num" }, w.raw.cpu_seconds ? fmtDur(w.raw.cpu_seconds) + " CPU" : "–")),
        h("td", {}, isActive(w) ? usageCell(w.use?.mem, w.res.mem, fmtBytes, " limit", true) : h("span", { class: "muted num" }, w.raw.peak_mem ? "peak " + fmtBytes(w.raw.peak_mem) : "–")),
        showCPUs && h("td", { class: "mono nowrap" }, w.cpus || "–"),
        h("td", { class: "nowrap" }, w.repo ? truncate(w.repo, 28) : h("span", { class: "muted" }, "–")),
        h("td", { class: "nowrap" }, w.caller || h("span", { class: "muted" }, "–")),
        h("td", { class: "nowrap num muted" }, t));
    })))));
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
function closeDrawer() { location.hash = "#/containers"; }
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
  head.replaceChildren(
    h("div", { class: "row" }, h("span", { class: "kind" }, w.kind === "job" ? "Job" : "Session"), h("h2", { class: "mono" }, w.id), exitChip(w),
      isActive(w) ? stopBtn(w) : null, closeBtn()),
    h("div", { class: "cmdline mono" }, w.kind === "job" ? w.cmd : "profile " + w.cmd));
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
  const b = h("button", { class: "btn danger" + (confirming ? " confirm" : ""), style: { marginLeft: "auto" } }, confirming ? (w.kind === "job" ? "Confirm cancel" : "Confirm stop") : (w.kind === "job" ? "Cancel job" : "Stop session"));
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
  if (isActive(w) && w.state === "running") {
    const hist = state.usage.get(w.runner + "/" + w.id) || [];
    out.push(h("div", { class: "sparks" },
      h("div", { class: "spark" }, h("div", { class: "k" }, "CPU now"), h("div", { class: "v num" }, w.use ? fmtCores(w.use.cpu) + (fmtCores(w.use.cpu) === "1" ? " core" : " cores") : "–", h("span", { class: "muted", style: { fontSize: "12px", fontWeight: 400 } }, `  of ${fmtCores(res.cpu)} guaranteed, ${fmtCores(res.cpu_max)} max`)),
        sparkline(hist, "cpu", "--series-2", fmtCores, res.cpu_max)),
      h("div", { class: "spark" }, h("div", { class: "k" }, "Memory now"), h("div", { class: "v num" }, w.use ? fmtBytes(w.use.mem) : "–", h("span", { class: "muted", style: { fontSize: "12px", fontWeight: 400 } }, `  of ${fmtBytes(res.mem)}${w.use?.mem_peak ? ", peak " + fmtBytes(w.use.mem_peak) : ""}`)),
        sparkline(hist, "mem", "--series-2", fmtBytes, res.mem))));
  }
  const rows = [];
  const add = (k, v) => { if (v != null && v !== "" && v !== false) rows.push(h("dt", {}, k), h("dd", {}, v)); };
  add("Runner", w.runner);
  if (r.reason) add("Reason", r.reason.replace(/^waiting: /, "Waiting: "));
  if (w.kind === "job") {
    const sp = r.spec;
    add("Profile", sp.profile);
    add("Repository", sp.workspace_key ? h("span", { class: "mono" }, sp.workspace_key) : "none (throwaway directory)");
    add("Directory", sp.dir && sp.dir !== "." ? h("span", { class: "mono" }, sp.dir) : null);
    add("Git", sp.git ? h("span", { class: "mono" }, (sp.git.branch ? sp.git.branch + " @ " : "") + sp.git.head.slice(0, 12)) : null);
    const up = r.uploaded || 0;
    add("Synced files", r.sync_files ? `${r.sync_files} · ` + (up === 0 ? "all already on the runner" : up === r.sync_files ? "all uploaded" : `${up} uploaded, ${r.sync_files - up} already on the runner`) : null);
  } else {
    add("Profile", r.profile);
    add("Clients attached", String(r.attached));
    add("Last client contact", r.last_seen ? ago(r.last_seen) : null);
    add("Network", r.network === "client" ? "through the attached client" : null);
  }
  add("Caller", [w.raw.caller, w.kind === "job" ? r.spec.client : r.client].filter(Boolean).filter((v, i, a) => a.indexOf(v) === i).join(" · "));
  add("Runtime", w.runtime === "container" ? h("span", {}, "container ", h("span", { class: "mono muted" }, w.image || "")) : w.runtime);
  add("Resources", `${fmtCores(res.cpu)} ${fmtCores(res.cpu) === "1" ? "core" : "cores"} guaranteed, up to ${fmtCores(res.cpu_max)} · ${fmtBytes(res.mem)} memory` + (r.mem_from ? ` (${r.mem_from === "learned" ? "learned from earlier runs" : r.mem_from})` : ""));
  add("Pinned CPUs", w.cpus ? h("span", { class: "mono" }, w.cpus) : null);
  add("Created", fmtTime(w.created) + " (" + ago(w.created) + ")");
  if (w.kind === "job" && r.started_at) add("Waited", fmtDur((new Date(r.started_at) - new Date(r.created_at)) / 1000));
  if (w.finished && w.started) add("Ran for", fmtDur((new Date(w.finished) - new Date(w.started)) / 1000));
  else if (w.started && w.state === "running") add("Running for", fmtDur(since(w.started)));
  if (r.exit_code != null) add("Exit code", String(r.exit_code) + (r.signal ? " (" + r.signal + ")" : ""));
  if (r.peak_mem) add("Peak memory", fmtBytes(r.peak_mem) + (res.mem ? ` (${pct(r.peak_mem / res.mem)} of the limit)` : ""));
  if (r.cpu_seconds) add("CPU time", fmtDur(r.cpu_seconds));
  if (w.kind === "job") {
    add("Secrets", (r.spec.secret_names || []).length ? r.spec.secret_names.join(", ") : null);
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
    let size = 0, preamble = "", partial = false, held = "";
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
        text = preamble.slice(nl + 1);
        preamble = null;
        partial = off > 0; // the tail starts mid-line: drop up to the first newline
      }
      if (partial) { // skip the cut-off line, unless no line ends soon (one huge line): then show it
        held += text;
        const nl = held.indexOf("\n");
        if (nl < 0 && held.length < 64 * 1024) continue;
        text = nl < 0 ? held : held.slice(nl + 1);
        partial = false; held = "";
      }
      text = stripAnsi(text);
      size += text.length;
      v.pre.append(document.createTextNode(text));
      while (size > 2 * 1024 * 1024 && v.pre.childNodes.length > 1) { size -= v.pre.firstChild.textContent.length; v.pre.firstChild.remove(); }
      if (v.follow) v.pre.scrollTop = v.pre.scrollHeight;
    }
    if (held && logView === v) v.pre.append(document.createTextNode(stripAnsi(held))); // it never reached a line end
    if (logView === v && v.abort === ac) v.status.textContent = "Ended";
  } catch (e) {
    if (e.name !== "AbortError" && logView === v) v.status.textContent = "Disconnected: " + e.message;
  }
}

// ---------- start ----------
window.addEventListener("resize", () => { if (state.view === "overview") renderCharts(); });
route();
pollFleet();
