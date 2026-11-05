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

let cur = null, curKind = "", tools = {}, permTimer = null;

function block(cls, text) {
  const near = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 50;
  const d = el("div", text, cls);
  logEl.append(d);
  cur = null; curKind = "";
  if (near) logEl.scrollTop = logEl.scrollHeight;
  return d;
}

function setTool(u) {
  const t = tools[u.toolCallId];
  if (!t) return;
  if (u.title) t.title = u.title;
  if (u.status) t.status = u.status;
  t.el.textContent = `tool: ${t.title}${t.status ? ` [${t.status}]` : ""}`;
}

function render(ev) {
  switch (ev.type) {
    case "user_message": block("user", `${ev.by}: ${ev.text}`); break;
    case "acp": {
      const u = ev.update || {};
      switch (u.sessionUpdate) {
        case "agent_message_chunk":
        case "agent_thought_chunk": {
          if (u.content?.type !== "text") break;
          const kind = u.sessionUpdate === "agent_message_chunk" ? "agent" : "thought";
          if (curKind !== kind) { const d = block(kind, ""); cur = d; curKind = kind; }
          cur.textContent += u.content.text;
          if (logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 200) logEl.scrollTop = logEl.scrollHeight;
          break;
        }
        case "tool_call":
          tools[u.toolCallId] = { el: block("tool", ""), title: "", status: "" };
          setTool(u);
          break;
        case "tool_call_update": setTool(u); break;
        case "plan":
          block("event", "plan:\n" + (u.entries || []).map((e) => `[${e.status}] ${e.content}`).join("\n"));
          break;
      }
      break;
    }
    case "permission_request":
      block("event", `permission ${ev.id}: ${ev.tool_call?.title || ""}`); schedulePerms(); break;
    case "question":
      block("event", `question ${ev.id}: ${ev.request?.message || ""}`); schedulePerms(); break;
    case "permission_resolved":
      block("event", `${ev.by || "agentd"} answered ${ev.id}: ${ev.error || ev.option}`); schedulePerms(); break;
    case "turn_end":
      block("event", ev.error ? `turn failed: ${ev.error}` : `turn ended${ev.stop_reason ? ` (${ev.stop_reason})` : ""}`);
      break;
    case "config_changed": block("event", `${ev.by} set ${JSON.stringify(ev.values)}`); break;
    case "state":
      $("#state").textContent = ev.state;
      $("#state").className = "state " + ev.state;
      if (ev.state === "error") block("event", `error: ${ev.error}`);
      break;
  }
}

async function open(backend, id) {
  current?.es?.close();
  logEl.replaceChildren(); pendingEl.replaceChildren();
  cur = null; curKind = ""; tools = {};
  $("#new").hidden = true;
  $("#send").hidden = false;
  const h = $("#header");
  h.className = "";
  const title = el("b", id), info = el("div", backend, "muted");
  h.replaceChildren(title, " ", el("span", "", "state"), info);
  h.querySelector(".state").id = "state";
  const es = new EventSource(`/api/b/${encodeURIComponent(backend)}/v1/agents/${id}/events?since=0`);
  es.onmessage = (m) => render(JSON.parse(m.data));
  es.onerror = () => { if (es.readyState === EventSource.CLOSED) checkSession(); };
  current = { backend, id, es };
  refreshList();
  try {
    const a = await api(backend, `/v1/agents/${id}`);
    title.textContent = a.name || id;
    info.textContent = `${backend} · ${a.harness} · ${a.cwd}${a.model ? " · " + a.model : ""} · ${a.owner}`;
  } catch (e) { info.textContent = e.message; }
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
