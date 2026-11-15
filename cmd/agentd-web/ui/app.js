// agentd-web UI: agent list across backends, live transcript, messages,
// pending permissions and questions. Agent output is only ever set as
// textContent, never as HTML.
const $ = (s) => document.querySelector(s);
const logEl = $("#log"), pendingEl = $("#pending");
let backends = [];
let current = null; // {backend, id, es}

function el(tag, text, cls) {
  const e = document.createElement(tag);
  if (text !== undefined) e.textContent = text;
  if (cls) e.className = cls;
  return e;
}

// api calls <backend>'s agentd API through the proxy and returns the parsed
// JSON body (or null). A 401 without a valid session starts a new login.
async function api(backend, path, method = "GET", body) {
  const r = await fetch(`/api/b/${encodeURIComponent(backend)}${path}`, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (r.status === 401) await checkSession();
  const text = await r.text();
  if (!r.ok) {
    let msg = text;
    try { msg = JSON.parse(text).error || text; } catch {}
    throw new Error(`${backend}: ${r.status} ${msg}`);
  }
  return text ? JSON.parse(text) : null;
}

// busy runs fn with form's buttons disabled, so a slow request can't be
// sent twice by clicking again.
async function busy(form, fn) {
  const buttons = [...form.querySelectorAll("button")];
  if (buttons.some((b) => b.disabled)) return;
  buttons.forEach((b) => (b.disabled = true));
  try { await fn(); } finally { buttons.forEach((b) => (b.disabled = false)); }
}

async function checkSession() {
  const r = await fetch("/api/backends");
  if (r.status === 401) location = "/login";
  return r;
}

// --- agent list ------------------------------------------------------------

async function refreshList() {
  const all = $("#showStopped").checked ? "?all=1" : "";
  const parts = await Promise.all(backends.map(async (b) => {
    try { return { b, agents: await api(b, "/v1/agents" + all) }; }
    catch (e) { return { b, err: e.message }; }
  }));
  const out = [];
  for (const { b, agents, err } of parts) {
    out.push(el("h3", b));
    if (err) { out.push(el("div", err, "muted")); continue; }
    if (!agents.length) out.push(el("div", "no agents", "muted"));
    agents.sort((x, y) => y.updated.localeCompare(x.updated));
    for (const a of agents) {
      const link = el("a");
      link.href = `#${encodeURIComponent(b)}/${a.id}`;
      if (current && current.backend === b && current.id === a.id) link.className = "active";
      link.append(el("span", a.name || a.id), " ", el("span", a.state, "state " + a.state));
      if (a.pending_permissions) link.append(" ", el("span", `${a.pending_permissions} pending`, "state waiting"));
      link.append(el("div", `${a.harness} · ${a.cwd}`, "muted"));
      out.push(link);
    }
  }
  $("#agents").replaceChildren(...out);
}

// --- one agent ---------------------------------------------------------------

// The transcript opens with the newest PAGE items of agentd's compacted
// history, then follows the raw events live. Scrolling to the top prepends
// the page before. Live events are queued and rendered once per frame.
const PAGE = 100;
let tools = {}, permTimer = null, queue = [], frame = 0;

// A sink is where render appends: its parent (#log, or a fragment for an
// earlier page), the message block that text chunks of the same kind
// continue, and whether its events are live (they update state, config and
// pending permissions) or older than those shown (they keep later tool
// calls of the same id).
function sink(parent, live, older) { return { parent, cur: null, curKind: "", live, older }; }

function block(s, cls, text) {
  const d = el("div", text, cls);
  s.parent.append(d);
  s.cur = null; s.curKind = "";
  return d;
}

// A tool call is a collapsed <details>: the summary shows title and
// status, expanding it shows the call's content (command output, diffs).
// Each update's content replaces the previous one (ACP).
function newTool(s, id) {
  const det = el("details");
  det.append(el("summary"), el("pre"));
  block(s, "tool", "").append(det);
  const t = { el: det, title: "", status: "" };
  if (!s.older || !tools[id]) tools[id] = t;
  return t;
}

function setTool(t, u) {
  if (!t) return;
  if (u.title) t.title = u.title;
  if (u.status) t.status = u.status;
  t.el.firstChild.textContent = `tool: ${t.title}${t.status ? ` [${t.status}]` : ""}`;
  if (u.content) {
    t.el.lastChild.textContent = u.content.map((c) =>
      c.type === "diff" ? `${c.path}:\n${c.newText ?? ""}`
      : c.type === "terminal" ? `(terminal ${c.terminalId})`
      : c.content?.type === "text" ? c.content.text : "").filter(Boolean).join("\n");
  }
}

function setState(state) {
  $("#state").textContent = state;
  $("#state").className = "state " + state;
}

function render(s, ev) {
  switch (ev.type) {
    case "user_message": block(s, "user", `${ev.by}: ${ev.text}`); break;
    case "acp": {
      const u = ev.update || {};
      switch (u.sessionUpdate) {
        case "agent_message_chunk":
        case "agent_thought_chunk": {
          if (u.content?.type !== "text") break;
          const kind = u.sessionUpdate === "agent_message_chunk" ? "agent" : "thought";
          if (s.curKind !== kind) { s.cur = block(s, kind, ""); s.curKind = kind; }
          s.cur.textContent += u.content.text;
          break;
        }
        case "tool_call": setTool(newTool(s, u.toolCallId), u); break;
        // In an earlier page an update belongs to no tool call shown.
        case "tool_call_update": if (!s.older) setTool(tools[u.toolCallId], u); break;
        case "plan":
          block(s, "event", "plan:\n" + (u.entries || []).map((e) => `[${e.status}] ${e.content}`).join("\n"));
          break;
      }
      break;
    }
    case "permission_request":
      block(s, "event", `permission ${ev.id}: ${ev.tool_call?.title || ""}`); if (s.live) schedulePerms(); break;
    case "question":
      block(s, "event", `question ${ev.id}: ${ev.request?.message || ""}`); if (s.live) schedulePerms(); break;
    case "permission_resolved":
      block(s, "event", `${ev.by || "agentd"} answered ${ev.id}: ${ev.error || ev.option}`); if (s.live) schedulePerms(); break;
    case "turn_end":
      block(s, "event", ev.error ? `turn failed: ${ev.error}` : `turn ended${ev.stop_reason ? ` (${ev.stop_reason})` : ""}`);
      break;
    case "config_changed":
      block(s, "event", `${ev.by} set ${JSON.stringify(ev.values)}`);
      if (s.live && current) loadConfig(current.backend, current.id);
      break;
    case "state":
      if (s.live) setState(ev.state);
      if (ev.state === "error") block(s, "event", `error: ${ev.error}`);
      break;
  }
}

// Setting scrollTop beyond the end scrolls to the end, without reading
// the layout.
function scrollToEnd() { logEl.scrollTop = 1e9; }

// flush renders the queued live events: one layout read before, one
// scroll after, and only if the view was at the bottom.
function flush() {
  frame = 0;
  const evs = queue;
  queue = [];
  if (!current?.live) return;
  const atBottom = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 50;
  for (const ev of evs) render(current.live, ev);
  if (atBottom) scrollToEnd();
}

// history fetches a page of agentd's compacted history.
function history(c, before) {
  return api(c.backend, `/v1/agents/${c.id}/history?compact=1&limit=${PAGE}${before ? `&before=${before}` : ""}`);
}

// loadEarlier prepends the page before the oldest item shown; what is on
// screen stays where it is.
async function loadEarlier() {
  const c = current;
  if (!c?.before || c.loading) return;
  c.loading = true;
  const s = sink(document.createDocumentFragment(), false, true);
  let before = 0;
  try {
    const h = await history(c, c.before);
    for (const ev of h.events) render(s, ev);
    before = h.before;
  } catch (e) { block(s, "event", e.message); }
  c.loading = false;
  if (current !== c) return;
  const height = logEl.scrollHeight;
  logEl.prepend(s.parent);
  logEl.scrollTop += logEl.scrollHeight - height;
  c.before = before;
  if (logEl.scrollTop < 200) loadEarlier();
}
logEl.onscroll = () => { if (logEl.scrollTop < 200) loadEarlier(); };

async function open(backend, id) {
  current?.es?.close();
  logEl.replaceChildren(); pendingEl.replaceChildren();
  tools = {}; queue = [];
  $("#new").hidden = true;
  $("#send").hidden = false;
  const h = $("#header");
  h.className = "";
  const title = el("b", id), info = el("div", backend, "muted");
  const cfg = el("div");
  cfg.id = "config";
  h.replaceChildren(title, " ", el("span", "", "state"), info, cfg);
  h.querySelector(".state").id = "state";
  // live: the sink of live events once the newest page is shown;
  // before: the cursor of the page before the oldest shown (0: none).
  const c = current = { backend, id, es: null, live: null, before: 0, loading: false };
  refreshList();
  loadPerms();
  api(backend, `/v1/agents/${id}`).then((a) => {
    title.textContent = a.name || id;
    info.textContent = `${backend} · ${a.harness} · ${a.cwd} · ${a.owner}`;
    showConfig(backend, id, a.config || []);
  }).catch((e) => { info.textContent = e.message; });

  let page;
  try { page = await history(c); }
  catch (e) { if (current === c) info.textContent = e.message; return; }
  if (current !== c) return;
  setState(page.state);
  const s = sink(logEl, false, false);
  for (const ev of page.events) render(s, ev);
  s.live = true;
  c.live = s;
  c.before = page.before;
  scrollToEnd();
  // Live events continue right after the page.
  const es = c.es = new EventSource(`/api/b/${encodeURIComponent(backend)}/v1/agents/${id}/events?since=${page.next}`);
  es.onmessage = (m) => {
    if (current !== c) return;
    queue.push(JSON.parse(m.data));
    frame ||= requestAnimationFrame(flush);
  };
  es.onerror = () => { if (es.readyState === EventSource.CLOSED) checkSession(); };
  if (logEl.scrollTop < 200) loadEarlier();
}

// showConfig renders the harness's config options (model, effort, mode, …)
// as controls; a change is sent as PATCH .../config, which resumes a
// stopped agent.
function showConfig(backend, id, options) {
  const box = $("#config");
  if (!box || current?.id !== id) return;
  box.replaceChildren(...options.map((o) => {
    const label = el("label", (o.name || o.id) + " ");
    let input;
    if (o.type === "boolean") {
      input = el("input");
      input.type = "checkbox";
      input.checked = o.value === true;
    } else {
      input = el("select");
      const cur = typeof o.value === "string" ? o.value : JSON.stringify(o.value);
      const values = o.options?.length ? o.options : [cur];
      if (!values.includes(cur)) values.unshift(cur);
      for (const v of values) { const opt = el("option", v); opt.value = v; input.append(opt); }
      input.value = cur;
    }
    input.onchange = async () => {
      input.disabled = true;
      const value = o.type === "boolean" ? input.checked : input.value;
      try { showConfig(backend, id, await api(backend, `/v1/agents/${id}/config`, "PATCH", { [o.id]: value })); }
      catch (e) { alert(e.message); loadConfig(backend, id); }
    };
    label.append(input);
    return label;
  }));
}

async function loadConfig(backend, id) {
  const cfg = await api(backend, `/v1/agents/${id}/config`).catch(() => null);
  if (cfg) showConfig(backend, id, cfg);
}

function schedulePerms() {
  if (!permTimer) permTimer = setTimeout(loadPerms, 200);
}

async function loadPerms() {
  permTimer = null;
  if (!current) return;
  const { backend, id } = current;
  const list = await api(backend, `/v1/agents/${id}/permissions`).catch((e) => [{ error: e.message }]);
  if (current?.id === id) pendingEl.replaceChildren(...list.map((p) => pendingForm(backend, id, p)));
}

async function answer(backend, id, req, body) {
  try { await api(backend, `/v1/agents/${id}/permissions/${req}`, "POST", body); }
  catch (e) { alert(e.message); }
  loadPerms();
}

function pendingForm(backend, id, p) {
  const d = el("div");
  if (p.error) { d.textContent = p.error; return d; }
  if (p.type === "permission_request") {
    const tc = p.tool_call || {};
    d.append(el("b", `Permission: ${tc.title || p.id}`));
    if (tc.rawInput) d.append(el("pre", JSON.stringify(tc.rawInput, null, 2)));
    for (const o of p.options || []) {
      const b = el("button", o.name || o.id);
      b.onclick = () => answer(backend, id, p.id, { option: o.id });
      d.append(b, " ");
    }
    return d;
  }
  // A question: an ACP form elicitation with a JSON schema.
  const req = p.request || {};
  const schema = req.requestedSchema || {};
  d.append(el("b", req.message || "Question"));
  const form = el("form");
  const fields = [];
  for (const [k, s] of Object.entries(schema.properties || {})) {
    const label = el("label", s.title || k);
    label.style.display = "block";
    if (s.description) label.append(el("div", s.description, "muted"));
    const opts = s.oneOf || s.anyOf || s.items?.oneOf || s.items?.anyOf;
    const values = opts ? opts.map((o) => [o.const, o.title || o.const])
      : (s.enum || s.items?.enum)?.map((v) => [v, v]);
    let input;
    if (values) {
      input = el("select");
      input.multiple = s.type === "array";
      if (!input.multiple) input.append(el("option", ""));
      for (const [v, t] of values) { const o = el("option", t); o.value = v; input.append(o); }
    } else if (s.type === "boolean") {
      input = el("input"); input.type = "checkbox";
    } else {
      input = el("input");
      input.type = s.type === "number" || s.type === "integer" ? "number" : "text";
    }
    label.append(" ", input);
    form.append(label);
    fields.push([k, s, input]);
  }
  const ok = el("button", "Answer");
  const decline = el("button", "Decline");
  decline.type = "button";
  decline.onclick = () => answer(backend, id, p.id, { decline: true });
  form.append(ok, " ", decline);
  form.onsubmit = (e) => {
    e.preventDefault();
    const out = {};
    for (const [k, s, input] of fields) {
      if (s.type === "boolean") out[k] = input.checked;
      else if (input.multiple) out[k] = [...input.selectedOptions].map((o) => o.value);
      else if (input.value !== "") out[k] = s.type === "number" || s.type === "integer" ? Number(input.value) : input.value;
    }
    answer(backend, id, p.id, { answer: out });
  };
  d.append(form);
  return d;
}

$("#send").onsubmit = async (e) => {
  e.preventDefault();
  const t = e.target.text;
  if (!current || !t.value.trim()) return;
  await busy(e.target, async () => {
    try { await api(current.backend, `/v1/agents/${current.id}/messages`, "POST", { text: t.value }); t.value = ""; }
    catch (err) { alert(err.message); }
  });
};
$("#send").text.onkeydown = (e) => { if (e.key === "Enter" && e.ctrlKey) $("#send").requestSubmit(); };
$("#cancel").onclick = () => current && api(current.backend, `/v1/agents/${current.id}/cancel`, "POST").catch((e) => alert(e.message));
// Stops the agent's process; its history stays and a new message resumes it.
$("#stop").onclick = () => current && busy($("#send"), async () => {
  try { await api(current.backend, `/v1/agents/${current.id}`, "DELETE"); await refreshList(); }
  catch (e) { alert(e.message); }
});

// --- new agent -------------------------------------------------------------

const newForm = $("#new");
async function loadHarnesses() {
  const hs = await api(newForm.backend.value, "/v1/harnesses").catch((e) => { alert(e.message); return []; });
  newForm.harness.replaceChildren(...hs.map((h) => el("option", h.name)));
}
$("#newBtn").onclick = () => {
  if (current) { current.es?.close(); current = null; location.hash = ""; }
  logEl.replaceChildren(); pendingEl.replaceChildren();
  $("#send").hidden = true;
  $("#header").textContent = "New agent";
  newForm.hidden = false;
  newForm.backend.replaceChildren(...backends.map((b) => el("option", b)));
  loadHarnesses();
};
newForm.backend.onchange = loadHarnesses;
newForm.onsubmit = async (e) => {
  e.preventDefault();
  const body = { harness: newForm.harness.value, cwd: newForm.cwd.value };
  if (newForm.agentName.value) body.name = newForm.agentName.value;
  if (newForm.prompt.value) body.prompt = newForm.prompt.value;
  const backend = newForm.backend.value;
  await busy(newForm, async () => {
    try {
      const a = await api(backend, "/v1/agents", "POST", body);
      newForm.reset();
      location.hash = `#${encodeURIComponent(backend)}/${a.id}`;
    } catch (err) { alert(err.message); }
  });
};

// --- start -------------------------------------------------------------------

function fromHash() {
  const m = location.hash.match(/^#([^/]+)\/(.+)$/);
  if (m) open(decodeURIComponent(m[1]), m[2]);
}
window.onhashchange = fromHash;
$("#showStopped").onchange = refreshList;

(async () => {
  const r = await checkSession();
  if (!r.ok) return;
  backends = await r.json();
  await refreshList();
  setInterval(refreshList, 5000);
  fromHash();
})();
