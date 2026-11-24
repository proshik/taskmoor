// taskmoor board: a client of taskmoor serve's API (snapshot + a stream of
// events). Every text from tasks and agents goes into the page through
// textContent, never as HTML.
//
// Sign-in: the one-time code arrives in the URL fragment (never sent to a
// server); the page trades it for a browser token and keeps that in
// localStorage, which belongs to this origin (host and port) alone. Every
// request carries it in the Authorization header: there is no cookie.
"use strict";

const COLUMNS = [
  ["ideas", "Ideas"], ["backlog", "Backlog"], ["ready", "Ready"], ["running", "Running"],
  ["needs", "Needs you"], ["review", "Review"], ["merged", "Merged"], ["done", "Done"],
];
const ENDED = new Set(["complete", "error", "stopped", "stale", "handedOver", "accepted"]);

const S = {
  snap: null,
  accept: {},           // acceptance views by session id, loaded on request
  ideaReview: {},       // intake reviews by idea id, loaded on request
  project: localGet("project"),
  selected: null,       // {project, task} whose panel is open
  inbox: false,         // the inbox panel is open
  body: null,           // {project, task, text} of the open card
  feed: null,           // {project, task, session, bySeq: Map}
  feedError: {},        // session id → time of the last failed load
  drafts: {},           // what the person typed or picked, by control key
  connected: false,
  gen: 0,               // the event stream's generation
  refreshing: false,
  refreshAgain: false,
  reloadFeed: false,
};

function localGet(k) { try { return localStorage.getItem("taskmoor." + k); } catch { return null; } }
function localSet(k, v) { try { if (v === null) localStorage.removeItem("taskmoor." + k); else localStorage.setItem("taskmoor." + k, v); return true; } catch { return false; /* private window, full storage */ } }

// h builds an element: h("div", {class: "x", onclick: f}, child, "text", …)
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid === null || kid === undefined || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

// nodes flattens a list of children and drops the empty ones.
function nodes(list) { return list.flat(Infinity).filter(k => k !== null && k !== undefined && k !== false); }

// ---- sign-in and requests ----

// signIn redeems a code from the fragment, at load and when a link is
// pasted into this tab (hashchange); one redemption per code.
async function signIn() {
  const m = /^#code=([0-9a-f]+)$/.exec(location.hash);
  if (!m || S.signingIn === m[1]) return;
  S.signingIn = m[1];
  history.replaceState(null, "", location.pathname); // the code leaves the address bar
  try {
    const resp = await fetch("/api/sign-in", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ code: m[1] }) });
    const data = await resp.json().catch(() => ({}));
    if (resp.ok && data.token) { localSet("token", data.token); showBoard(); return; }
    signinMessage("Sign-in failed: " + (data.error || resp.statusText));
  } catch (e) {
    signinMessage("Sign-in failed: taskmoor serve did not answer (" + e.message + ").");
  } finally {
    S.signingIn = null;
  }
}

function signinMessage(text) {
  showSignin();
  document.getElementById("signin-msg").textContent = text;
}

class Unauthorized extends Error {}

// From outside (stage 9) the board is a paired device: its token, kept in
// this origin's storage, and the device's passkey for each Accept, Approve
// and allowed request (the service answers 428 and names what to confirm).
function credKey() { return S.remote ? "device" : "token"; }
function authHeader() { return (S.remote ? "Device " : "Bearer ") + (localGet(credKey()) || ""); }

async function api(method, path, body, stepup) {
  const cred = localGet(credKey());
  if (!cred) { showSignin(); throw new Unauthorized("not signed in"); }
  const opts = { method, headers: { "Authorization": authHeader() } };
  if (stepup) opts.headers["X-Taskmoor-Stepup"] = stepup;
  if (method !== "GET") {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body || {});
  }
  const resp = await fetch(path, opts);
  let data = null;
  try { data = await resp.json(); } catch { /* empty */ }
  if (resp.status === 401 && S.remote && resp.headers.get("X-Taskmoor-Login") === "required") { outerSignIn(); throw new Unauthorized("sign in again"); }
  if (resp.status === 401) {
    // only the credential it was sent with is refused: a newer one stays
    if (localGet(credKey()) === cred) signedOut();
    throw new Unauthorized("not signed in");
  }
  if (resp.status === 428 && S.remote && data && data.stepup && !stepup) {
    return api(method, path, body, await stepUp(data.stepup));
  }
  if (!resp.ok) throw new Error((data && data.error) || resp.statusText);
  return data;
}

// ---- notifications (stage 10) ----

function pushSupported() { return "serviceWorker" in navigator && "PushManager" in window && "Notification" in window; }

// pushControl: this device's notifications, on or off.
function pushControl() {
  const box = h("div", { class: "push" });
  const err = h("span", { class: "err" });
  if (!pushSupported()) {
    box.replaceChildren(h("span", { class: "desc" }, "Notifications: this browser has none here. On an iPhone, add the board to the Home Screen (Share → Add to Home Screen) and open it from there."));
    return box;
  }
  const draw = async () => {
    let on = false;
    try {
      const reg = await navigator.serviceWorker.ready;
      const sub = await reg.pushManager.getSubscription();
      if (sub) on = !!(await api("GET", "/api/push/key?endpoint=" + encodeURIComponent(sub.endpoint))).subscribed;
    } catch { /* shown as off */ }
    const denied = Notification.permission === "denied";
    box.replaceChildren(h("span", {}, "Notifications: " + (on ? "on" : denied ? "blocked in this browser's settings" : "off") + " "),
      denied ? null : h("button", { type: "button", class: on ? "" : "primary", onclick: async () => {
        err.textContent = "";
        try { if (on) await pushOff(); else await pushOn(); } catch (e) { err.textContent = e.message; }
        draw();
      } }, on ? "Turn off" : "Notify me"), " ", err);
  };
  draw();
  return box;
}

async function pushOn() {
  if (await Notification.requestPermission() !== "granted") throw new Error("notifications were not allowed");
  const reg = await navigator.serviceWorker.ready;
  const { key } = await api("GET", "/api/push/key");
  let sub = await reg.pushManager.getSubscription();
  if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64uToBuf(key) });
  const j = sub.toJSON();
  await api("POST", "/api/push/subscribe", { endpoint: j.endpoint, keys: j.keys });
}

async function pushOff() {
  const reg = await navigator.serviceWorker.ready;
  const sub = await reg.pushManager.getSubscription();
  if (!sub) return;
  await api("POST", "/api/push/unsubscribe", { endpoint: sub.endpoint });
  await sub.unsubscribe();
}

// outerSignIn: the VPS's sign-in has expired (the device stays paired):
// the whole page goes through it again.
function outerSignIn() {
  forgetBoard(); // refused by the VPS: nothing stays for an offline start
  location.assign("/oauth2/sign_in?rd=" + encodeURIComponent(location.href));
}

function b64uToBuf(s) {
  s = s.replace(/-/g, "+").replace(/_/g, "/");
  while (s.length % 4) s += "=";
  return Uint8Array.from(atob(s), c => c.charCodeAt(0)).buffer;
}
function bufToB64u(b) {
  let s = "";
  for (const x of new Uint8Array(b)) s += String.fromCharCode(x);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

async function postOpen(path, body) {
  const resp = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(data.error || resp.statusText);
  return data;
}

// pairDevice spends a code from the Mac and registers this device's passkey.
async function pairDevice(code, name) {
  const b = await postOpen("/api/device/pair/begin", { code, name });
  const pk = b.options.publicKey;
  pk.challenge = b64uToBuf(pk.challenge);
  pk.user.id = b64uToBuf(pk.user.id);
  for (const c of pk.excludeCredentials || []) c.id = b64uToBuf(c.id);
  const cred = await navigator.credentials.create({ publicKey: pk });
  const r = cred.response;
  const credential = { id: cred.id, rawId: bufToB64u(cred.rawId), type: cred.type,
    response: { clientDataJSON: bufToB64u(r.clientDataJSON), attestationObject: bufToB64u(r.attestationObject),
      transports: r.getTransports ? r.getTransports() : [] },
    clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {} };
  const f = await postOpen("/api/device/pair/finish", { ceremony: b.ceremony, credential });
  localSet("device", f.token);
}

// stepUp confirms one dangerous action with this device's passkey and
// returns the one-use grant for it.
async function stepUp(action) {
  const b = await api("POST", "/api/stepup/begin", { action });
  const pk = b.options.publicKey;
  pk.challenge = b64uToBuf(pk.challenge);
  for (const c of pk.allowCredentials || []) c.id = b64uToBuf(c.id);
  const a = await navigator.credentials.get({ publicKey: pk });
  const r = a.response;
  const assertion = { id: a.id, rawId: bufToB64u(a.rawId), type: a.type,
    response: { clientDataJSON: bufToB64u(r.clientDataJSON), authenticatorData: bufToB64u(r.authenticatorData),
      signature: bufToB64u(r.signature), userHandle: r.userHandle ? bufToB64u(r.userHandle) : null },
    clientExtensionResults: a.getClientExtensionResults ? a.getClientExtensionResults() : {} };
  const f = await api("POST", "/api/stepup/finish", { ceremony: b.ceremony, assertion });
  return f.stepup;
}

// signedOut: the credential was refused — nothing of the board stays, in
// memory or in this device's storage.
function signedOut() {
  localSet(credKey(), null);
  forgetBoard();
  showSignin();
}

// forgetBoard drops the board here and in every tab of this origin (the
// session generation changes, so their responses in flight are dropped
// too); the device stays paired.
function forgetBoard() {
  localSet("session", String(Date.now()) + Math.random());
  localSet("snap", null);
  clearBoard();
}

function clearBoard() {
  S.snap = null; S.feed = null; S.body = null; S.inbox = false; S.devices = false; S.selected = null;
  S.gen++; if (S.abort) { S.abort.abort(); S.abort = null; }
}

// signOutOutside: from outside, the VPS's sign-out, to a page outside the
// sign-in (so it does not sign straight back in); the board is forgotten
// first.
function signOutOutside() {
  forgetBoard();
  location.assign("/oauth2/sign_out?rd=" + encodeURIComponent("/signed-out"));
}

// another tab signed out, was refused or paired again: this one forgets
// what it shows, and reads again if there is still a credential
window.addEventListener("storage", (e) => {
  if (!["taskmoor.device", "taskmoor.token", "taskmoor.session"].includes(e.key)) return;
  clearBoard();
  if (localGet(credKey())) refresh(); else showSignin();
});

function showSignin() {
  clearBoard(); // no board, and no stream, without a credential
  S.connected = false;
  if (S.remote) pairView();
  document.getElementById("signin").hidden = false;
  document.getElementById("board").hidden = true;
  document.querySelector(".top").hidden = true;
  document.getElementById("panel").hidden = true;
}

// pairView: from outside, a device not paired yet pairs with a code shown
// on the Mac (taskmoor device pair).
function pairView() {
  const box = document.getElementById("signin");
  const msg = h("p", { id: "signin-msg", class: "err" });
  const code = h("input", { type: "text", placeholder: "Code from the Mac (taskmoor device pair)", autocomplete: "one-time-code", autocapitalize: "none" });
  const name = h("input", { type: "text", placeholder: "This device's name (e.g. phone)" });
  const btn = h("button", { type: "button", class: "primary", onclick: async () => {
    btn.disabled = true; msg.textContent = "";
    try { await pairDevice(code.value, name.value); showBoard(); refresh(); }
    catch (e) { msg.textContent = "Not paired: " + e.message; }
    btn.disabled = false;
  } }, "Pair and create a passkey");
  box.replaceChildren(h("h1", {}, "taskmoor"),
    h("p", {}, "This device is not paired. On the Mac run ", h("code", {}, "taskmoor device pair"), " and enter its code here; this device then creates a passkey, which confirms every Accept, Approve and allowed request from it."),
    code, name, btn, msg);
}

function showBoard() {
  document.getElementById("signin").hidden = true;
  document.getElementById("signin-msg").textContent = "";
  document.getElementById("board").hidden = false;
  document.querySelector(".top").hidden = false;
}

// ---- data ----

function currentProject() {
  const ps = (S.snap && S.snap.projects) || [];
  return ps.find(p => p.name === S.project) || ps[0] || null;
}

// latestSessions maps a task id to its newest session in the project.
function latestSessions(project) {
  const p = S.snap.projects.find(x => x.name === project);
  const m = new Map();
  for (const s of S.snap.sessions) {
    // a session of the same name's earlier repository is not this project's
    if (s.project !== project || (s.project_path && p && s.project_path !== p.path)) continue;
    if (s.restarted_as || s.abandoned) continue; // a Restart replaced it: the card is its successor's
    const cur = m.get(s.task);
    if (!cur || s.started_at > cur.started_at || s.started_at === cur.started_at && s.id > cur.id) m.set(s.task, s);
  }
  return m;
}

function column(t, sess) {
  switch (t.lifecycle) {
    case "backlog": return t.ready ? "ready" : "backlog";
    case "in_progress": return sess && sess.state === "awaitingInput" ? "needs" : "running";
    case "review": return "review";
    case "merged": return "merged";
    case "done": return "done";
  }
  return null; // cancelled: not on the board
}

// refresh reads a snapshot. Requests never overlap: an update that comes
// during one asks for another after it. A snapshot older than the one
// shown (same service run, lower revision) is dropped.
async function refresh() {
  if (S.refreshing) { S.refreshAgain = true; return; }
  S.refreshing = true;
  const cred = localGet(credKey()), session = localGet("session");
  try {
    const snap = await api("GET", "/api/snapshot");
    if (localGet(credKey()) !== cred || localGet("session") !== session) { S.refreshing = false; return; } // signed out meanwhile: dropped
    const old = S.snap;
    if (!(old && old.epoch === snap.epoch && snap.rev < old.rev)) S.snap = snap;
    // the last view, for opening the board with no network (stage 10)
    // too big for the device's storage: no copy rather than an old one
    try {
      const j = JSON.stringify(S.snap);
      if (j.length >= 2000000 || !localSet("snap", j)) localSet("snap", null); // no copy rather than an old one
    } catch { localSet("snap", null); }
  } catch (e) {
    S.refreshing = false;
    if (e instanceof Unauthorized) return;
    if (!S.snap) { // no network at all: the last view, marked Disconnected
      try { const c = JSON.parse(localGet("snap") || "null"); if (c && c.projects) { S.snap = c; showBoard(); render(); } } catch { /* none */ }
    }
    // the service is unreachable or failing: the last view stays, marked
    // Disconnected (never Ended), and the page keeps trying
    disconnected("Disconnected from taskmoor serve (" + e.message + ") — what you see may be out of date. Retrying…");
    return;
  }
  S.refreshing = false;
  banner(null);
  render();
  if (S.reloadFeed) { S.reloadFeed = false; reloadOpenFeed(); }
  connect();
  if (S.refreshAgain) { S.refreshAgain = false; refresh(); }
}

// scheduleRefresh refreshes at most 250 ms after the first of a burst of
// events: a busy stream cannot postpone it forever
function scheduleRefresh() {
  if (S.refreshTimer) return;
  S.refreshTimer = setTimeout(() => { S.refreshTimer = null; refresh(); }, 250);
}

function disconnected(text) {
  S.connected = false; conn(); banner(text);
  S.gen++; // the current stream, if any, is abandoned
  if (S.abort) { S.abort.abort(); S.abort = null; }
  S.reloadFeed = true; // events may be missed while away
  clearTimeout(S.retryTimer);
  S.retryTimer = setTimeout(refresh, 3000);
}

// connect follows the event stream from the snapshot's position, read with
// fetch so the token goes in a header. A lost stream shows Disconnected —
// never Ended — and reconnects. A stream of an older generation is ignored.
function connect() {
  if (S.abort && S.streamEpoch === S.snap.epoch) return;
  if (S.abort) S.abort.abort();
  const gen = ++S.gen;
  const ctl = new AbortController();
  S.abort = ctl; S.streamEpoch = S.snap.epoch;
  const auth = authHeader();
  const cred = localGet(credKey());
  (async () => {
    let resp;
    try {
      resp = await fetch(`/api/events?epoch=${encodeURIComponent(S.snap.epoch)}&rev=${S.snap.rev}`,
        { headers: { "Authorization": auth }, signal: ctl.signal });
    } catch { if (gen === S.gen) lost(); return; }
    if (resp.status === 401) {
      if (S.remote && resp.headers.get("X-Taskmoor-Login") === "required") { outerSignIn(); return; }
      if (localGet(credKey()) === cred) signedOut();
      return;
    }
    if (!resp.ok || !resp.body) { if (gen === S.gen) lost(); return; }
    if (gen !== S.gen) return;
    S.connected = true; conn(); banner(null);
    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        if (gen !== S.gen) return;
        buf += dec.decode(value, { stream: true });
        let i;
        while ((i = buf.indexOf("\n\n")) >= 0) {
          const block = buf.slice(0, i); buf = buf.slice(i + 2);
          for (const line of block.split("\n")) if (line.startsWith("data: ")) onEvent(line.slice(6));
          if (gen !== S.gen) return;
        }
      }
    } catch { /* aborted or broken */ }
    if (gen === S.gen) lost();
  })();
}

function lost() {
  S.abort = null;
  disconnected("Disconnected from taskmoor serve — what you see may be out of date. Reconnecting…");
}

function onEvent(data) {
  let ev;
  try { ev = JSON.parse(data); } catch { return; }
  if (ev.type === "reset") {
    // missed events: read everything again; the stream itself goes on
    S.reloadFeed = true;
    refresh();
    return;
  }
  if (ev.type === "activity" && S.feed && ev.data.session === S.feed.session) {
    addActivity(ev.data.activity);
    renderFeed();
    return;
  }
  scheduleRefresh();
}

// a stream that died without telling is retried
setInterval(() => { if (!S.connected && localGet(credKey())) refresh(); }, 5000);

function conn() {
  const el = document.getElementById("conn");
  el.className = "conn" + (S.connected ? "" : " off");
  el.title = S.connected ? "Connected to taskmoor serve" : "Disconnected";
}

function banner(text) {
  const el = document.getElementById("banner");
  el.hidden = !text;
  el.textContent = text || "";
}

// ---- drafts: what the person typed survives a redraw ----

function draft(el, key) {
  el.dataset.draft = key;
  if (key in S.drafts) {
    if (el.type === "checkbox" || el.type === "radio") el.checked = S.drafts[key];
    else el.value = S.drafts[key];
  }
  const save = () => {
    if (el.type === "radio") {
      document.querySelectorAll(`input[name="${CSS.escape(el.name)}"]`).forEach(r => { S.drafts[r.dataset.draft] = r.checked; });
    } else S.drafts[key] = (el.type === "checkbox") ? el.checked : el.value;
  };
  el.addEventListener("input", save);
  el.addEventListener("change", save);
  return el;
}

function forget(prefix) { for (const k of Object.keys(S.drafts)) if (k.startsWith(prefix)) delete S.drafts[k]; }

// keepFocus redraws while keeping the focused control and its caret.
function keepFocus(redraw) {
  const a = document.activeElement;
  const key = a && a.dataset && a.dataset.draft;
  const sel = key && "selectionStart" in a ? [a.selectionStart, a.selectionEnd] : null;
  redraw();
  if (!key) return;
  const el = document.querySelector(`[data-draft="${CSS.escape(key)}"]`);
  if (!el) return;
  el.focus();
  if (sel) { try { el.setSelectionRange(sel[0], sel[1]); } catch { /* not a text control */ } }
}

// ---- rendering ----

function render() {
  if (!S.snap) return; // nothing to show: signed out, or not loaded yet
  keepFocus(() => {
    const sel = document.getElementById("project");
    sel.replaceChildren(...S.snap.projects.map(p => h("option", { value: p.name, selected: p === currentProject() }, p.name)));
    const count = S.snap.asks.length;
    document.getElementById("inbox-count").textContent = count;
    document.getElementById("inbox-btn").className = "inbox-btn" + (count ? " has" : "");
    renderBoard();
    renderKeep(currentProject());
    renderUsage();
    renderPanel();
  });
}

function renderBoard() {
  const p = currentProject();
  const board = document.getElementById("board");
  if (!p) {
    board.replaceChildren(h("p", { class: "empty" }, "No project is registered. Run `taskmoor project add PATH`."));
    return;
  }
  document.getElementById("facts").textContent = p.path + (p.error ? " — " + p.error : "");
  const sessions = latestSessions(p.name);
  const cols = new Map(COLUMNS.map(([k]) => [k, []]));
  for (const t of p.tasks) {
    const c = column(t, sessions.get(t.id));
    if (c) cols.get(c).push(t);
  }
  const ideas = (S.snap.ideas || []).filter(i => i.project === p.name && (!i.project_path || i.project_path === p.path) &&
    (i.state === "new" || i.state === "grilling" || i.state === "proposed"));
  board.replaceChildren(...COLUMNS.map(([k, name]) => {
    if (k === "ideas") {
      return h("section", { class: "col ideas" },
        h("h2", {}, name + " ", h("span", {}, ideas.length)),
        h("button", { type: "button", class: "add-idea", onclick: () => openNewIdea(p.name) }, "+ New idea"),
        ideas.map(i => ideaCard(p.name, i)));
    }
    const tasks = cols.get(k).sort((a, b) => a.priority - b.priority || a.id.localeCompare(b.id));
    return h("section", { class: "col" + (k === "needs" ? " needs" : "") },
      h("h2", {}, name + " ", h("span", {}, tasks.length)),
      tasks.length ? tasks.map(t => card(p.name, t, sessions.get(t.id), k)) : h("div", { class: "empty" }, "—"));
  }));
}

function ideaSession(i) { return i.session ? S.snap.sessions.find(s => s.id === i.session) : null; }

function ideaCard(project, i) {
  const sess = ideaSession(i);
  const meta = [h("span", { class: "chip" }, i.state)];
  if (sess && !ENDED.has(sess.state)) meta.push(h("span", { class: "st-" + sess.state }, "● " + stateWord(sess.state)));
  if (i.tasks && i.tasks.length) meta.push(h("span", {}, i.tasks.join(", ")));
  const waiting = sess && sess.state === "awaitingInput";
  return h("article", {
    class: "card idea" + (waiting ? " needs" : "") + (isSelected(project, i.id) ? " sel" : ""),
    tabindex: "0", "data-idea": i.id,
    onclick: () => openIdea(project, i.id),
    onkeydown: (e) => { if (e.key === "Enter") openIdea(project, i.id); },
  },
  h("div", { class: "row" }, h("span", { class: "id" }, i.id)),
  h("div", { class: "title" }, i.title),
  h("div", { class: "meta" }, meta));
}

function openIdea(project, id) {
  const fresh = !isSelected(project, id);
  S.project = project; localSet("project", project);
  S.selected = { project, task: id, idea: true }; S.inbox = false; S.devices = false;
  if (fresh) { S.feed = null; S.body = null; }
  render();
  if (fresh) document.getElementById("panel").scrollTop = 0;
}

function openNewIdea(project) {
  S.project = project; localSet("project", project);
  S.selected = { project, task: "", newIdea: true }; S.inbox = false; S.devices = false; S.feed = null;
  render();
}

function newIdeaView(project) {
  const key = "idea:new:" + project;
  const title = draft(h("input", { type: "text", placeholder: "A short title (or the first line below)" }), key + ":title");
  const text = draft(h("textarea", { placeholder: "The raw idea, as it is…", rows: "8" }), key + ":text");
  const err = h("div", { class: "err" });
  const add = h("button", { type: "button", class: "primary", onclick: async () => {
    if (!text.value.trim() && !title.value.trim()) return;
    add.disabled = true; err.textContent = "";
    try {
      const i = await api("POST", "/api/ideas", { project, title: title.value, text: text.value });
      forget(key);
      await refresh();
      openIdea(project, i.id);
    } catch (e) { err.textContent = e.message; add.disabled = false; }
  } }, "Keep the idea");
  return [h("h1", {}, "New idea · " + project),
    h("p", { class: "desc" }, "Kept in taskmoor's store, outside git. Grilling it starts an agent that asks you about it, in forms, and writes the tasks it comes to on an intake branch; you approve them into main."),
    title, text, h("div", { class: "actions" }, add), err];
}

function ideaView(p, i) {
  const sess = ideaSession(i);
  const out = [h("h1", {}, i.id + "  " + i.title)];
  const facts = [["state", i.state]];
  if (i.branch) facts.push(["branch", i.branch], ["worktree", i.worktree]);
  if (i.tasks && i.tasks.length) facts.push(["tasks", i.tasks.join(", ")]);
  if (i.merged) facts.push(["merged", i.merged.slice(0, 12)]);
  out.push(h("dl", { class: "facts-list" }, facts.map(([k, v]) => [h("dt", {}, k), h("dd", {}, v)])));
  out.push(h("h3", {}, "The idea"), h("div", { class: "body" }, i.text || "(no text)"));
  const waiting = S.snap.asks.filter(a => a.session === i.session);
  if (waiting.length) {
    out.push(h("p", { class: "needs-note" }, "The grilling waits for you: " + waiting.length + " question(s) or approval(s). ",
      h("button", { type: "button", class: "primary", onclick: () => { S.inbox = true; S.devices = false; S.selected = null; render(); } }, "Answer in Needs you")));
  }
  const err = h("div", { class: "err" });
  if (i.state === "new") {
    const key = "grill:" + i.id;
    const select = draft(h("select", { "aria-label": "Profile" },
      h("option", { value: "" }, "Routed profile (tasks.toml routing.design, else the default)"),
      S.snap.profiles.map(pr => h("option", { value: pr.name, disabled: !pr.runnable }, pr.name + " (" + pr.harness + (pr.model ? ", " + pr.model : "") + (pr.api_billing ? ", API key" : "") + ")" + (pr.runnable ? "" : " — not runnable")))), key + ":profile");
    const go = async (anyway) => {
      const s = await api("POST", `/api/ideas/${encodeURIComponent(i.id)}/grill`, { profile: select.value, anyway });
      forget(key);
      await refresh();
      loadFeed(p.name, i.id, s.id);
    };
    const btn = h("button", { type: "button", class: "primary", onclick: async () => {
      btn.disabled = true; err.textContent = "";
      try { try { await go(false); } catch (e) { if (!offerAnyway(err, e, "Grill anyway", go)) throw e; } }
      catch (e) { err.textContent = e.message; }
      btn.disabled = false;
    } }, "Grill it");
    out.push(h("h3", {}, "Grill"), h("p", { class: "desc" }, "An agent reads the repository, asks you what it needs to know (the questions come to Needs you and Telegram), then writes the tasks on intake/" + i.id + "."),
      h("div", { class: "actions" }, select, btn));
  }
  if (i.state === "proposed" || i.state === "grilling") out.push(ideaReviewView(i));
  if (i.state !== "approved" && i.state !== "dropped") {
    out.push(h("div", { class: "actions" }, h("button", { type: "button", class: "danger", onclick: async () => {
      if (!confirmStep("Drop " + i.id + "? Its grilling stops; its branch and worktree stay.")) return;
      err.textContent = "";
      try { await api("POST", `/api/ideas/${encodeURIComponent(i.id)}/drop`, {}); await refresh(); }
      catch (e) { err.textContent = e.message; }
    } }, "Drop the idea")));
  }
  out.push(err);
  if (sess) out.push(sessionView(sess));
  return out;
}

// ideaReviewView is the intake's tasks and Approve (C9): loaded on request.
function ideaReviewView(i) {
  const box = h("div", { class: "acceptance" });
  const err = h("div", { class: "err" });
  const load = async () => {
    err.textContent = "";
    try { S.ideaReview[i.id] = await api("GET", `/api/ideas/${encodeURIComponent(i.id)}/review`); render(); }
    catch (e) { err.textContent = e.message; }
  };
  const rv = S.ideaReview[i.id];
  if (!rv) { box.replaceChildren(h("h3", {}, "Approve"), h("div", { class: "actions" }, h("button", { type: "button", onclick: load }, "Show the tasks to approve")), err); return box; }
  box.replaceChildren(...[h("h3", {}, "Approve"),
    h("p", {}, rv.ready ? "Ready to approve." : "Not ready: " + rv.why),
    h("h3", {}, "Tasks it adds"),
    rv.tasks.length ? h("ul", {}, rv.tasks.map(t => h("li", {}, h("span", { class: "mono" }, t.id), " " + t.title))) : h("p", { class: "empty" }, "None yet."),
    (rv.errors || []).length ? h("ul", { class: "reasons" }, rv.errors.map(e => h("li", { class: "err" }, e))) : null,
    (rv.warnings || []).length ? h("ul", { class: "reasons" }, rv.warnings.map(e => h("li", {}, e))) : null,
    rv.main_moved ? h("p", { class: "desc" }, rv.main_branch + " has moved since the branch was made: Approve sends it back to its session to merge it in and check the ids first.") : null,
    rv.result ? [h("h3", {}, "The agent's summary"), h("div", { class: "body" }, rv.result)] : null,
    h("details", {}, h("summary", {}, "Diff" + (rv.diff_cut ? " (cut)" : "")), h("pre", { class: "mono body diff" }, rv.diff)),
    h("div", { class: "actions" },
      h("button", { type: "button", class: "primary", disabled: !rv.ready, onclick: async () => {
        if (!confirmStep("Merge the tasks of " + i.branch + " at " + rv.tip.slice(0, 10) + " into " + rv.main_branch + "? Push stays yours.")) return;
        err.textContent = "";
        try {
          const r = await api("POST", `/api/ideas/${encodeURIComponent(i.id)}/approve`, { tip: rv.tip, main: rv.main });
          delete S.ideaReview[i.id];
          banner(r.sent_back ? rv.main_branch + " has moved: sent back to the session to bring the branch up to date; look again when it is done." : "Approved: merged as " + (r.merged || "").slice(0, 12) + ". Push is yours.");
          await refresh();
        } catch (e) { err.textContent = e.message; }
      } }, "Approve"),
      h("button", { type: "button", onclick: load }, "Refresh")),
    err].flat(Infinity).filter(x => x != null));
  return box;
}

// devicesView: on the Mac's board, the devices paired from outside (stage
// 9): a pairing code, the list, revoke.
async function loadDevices() {
  try { S.deviceList = await api("GET", "/api/devices"); } catch (e) { S.deviceErr = e.message; }
  render();
}

function devicesView() {
  const err = h("div", { class: "err" }, S.deviceErr || "");
  const out = [h("h1", {}, "Devices"),
    h("p", { class: "desc" }, "Devices reach this board from https://" + S.snap.remote_host + "/ through your VPS. Each one is paired once with a code from here and confirms every Accept, Approve and allowed request with its own passkey.")];
  const pairBtn = h("button", { type: "button", class: "primary", onclick: async () => {
    err.textContent = "";
    try { S.pairing = await api("POST", "/api/devices/pair", {}); S.pairingAt = Date.now(); render(); }
    catch (e) { err.textContent = e.message; }
  } }, "Pair a device");
  out.push(h("div", { class: "actions" }, pairBtn));
  if (S.pairing && Date.now() - S.pairingAt < 10 * 60 * 1000) {
    out.push(h("p", {}, "On the device open ", h("b", {}, S.pairing.url), ", sign in, and enter ", h("b", { class: "mono" }, S.pairing.code), " within 10 minutes (it works once)."));
  }
  const ds = S.deviceList || [];
  out.push(h("h3", {}, "Paired"));
  if (!ds.length) out.push(h("p", { class: "empty" }, "None."));
  for (const d of ds) {
    out.push(h("div", { class: "row" }, h("b", {}, d.name), " ", h("span", { class: "mono" }, d.id),
      " paired " + clock(d.created) + (d.last_seen ? ", last seen " + clock(d.last_seen) : ""), " ",
      h("button", { type: "button", class: "danger", onclick: async () => {
        if (!confirmStep("Revoke " + d.name + "? Its token and passkey stop working at once.")) return;
        try { await api("POST", `/api/devices/${encodeURIComponent(d.id)}/revoke`, {}); await loadDevices(); }
        catch (e) { err.textContent = e.message; }
      } }, "Revoke")));
  }
  out.push(err);
  return out;
}

function isSelected(project, task) { return S.selected && S.selected.project === project && S.selected.task === task; }

function card(project, t, sess, col) {
  const meta = [];
  if (t.needs_person) meta.push(h("span", { class: "chip", title: "needs a person" }, "⚑"));
  if (col === "backlog" && t.reasons.length) meta.push(h("span", {}, t.reasons[0]));
  if (sess && (col === "running" || col === "needs" || col === "review" || !ENDED.has(sess.state))) {
    meta.push(h("span", { class: "st-" + sess.state }, "● " + stateWord(sess.state)));
    if (sess.billing) meta.push(billingChip(sess.billing));
    if (!sess.confined) meta.push(isolationChip(sess.isolation));
    if (sess.review) meta.push(h("span", { class: "chip", title: "code review (revust)" }, "review " + sess.review.done + "/" + sess.review.rounds + " " + reviewState(sess.review, sess)));
  } else if (t.harness) {
    meta.push(h("span", { class: "st-" + t.harness.status }, "● " + t.harness.status));
  }
  if (sess && sess.review && ENDED.has(sess.state) && !(col === "running" || col === "needs" || col === "review") && reviewState(sess.review, sess) === "postponed, not run") {
    meta.push(h("span", { class: "chip", title: "its code review never ran" }, "review not run"));
  }
  if (t.state && t.state.stale) meta.push(h("span", { class: "chip" }, "stale"));
  if (t.release_tag) meta.push(h("span", { class: "chip" }, t.release_tag));
  return h("article", {
    class: "card" + (col === "needs" ? " needs" : "") + (isSelected(project, t.id) ? " sel" : ""),
    tabindex: "0", "data-task": t.id,
    onclick: () => openTask(project, t.id),
    onkeydown: (e) => { if (e.key === "Enter") openTask(project, t.id); },
  },
  h("div", { class: "row" }, h("span", { class: "id" }, t.id), h("span", { class: "chip" }, "P" + t.priority)),
  h("div", { class: "title" }, t.title),
  h("div", { class: "meta" }, meta));
}

// reviewState: a round the plan's guard held back is postponed (it starts
// on its own once the plan has room), not failed.
function reviewState(rv, sess) {
  // only a completed session's round starts again on its own (stage 6)
  if (rv.state === "error" && /^postponed:/.test(rv.error || "")) return sess && sess.state === "complete" ? "postponed" : "postponed, not run";
  return rv.state;
}

function stateWord(s) {
  return { awaitingInput: "waiting for you", active: "working", handedOver: "in your terminal" }[s] || s;
}

function billingChip(b) {
  return b === "api" ? h("span", { class: "chip api", title: "bills an API key" }, "API key")
    : h("span", { class: "chip sub", title: "your subscription" }, "subscription");
}

async function openTask(project, id) {
  const fresh = !isSelected(project, id);
  const gen = S.bodyGen = (S.bodyGen || 0) + 1; // a later open wins over an earlier response
  S.project = project; localSet("project", project);
  S.selected = { project, task: id }; S.inbox = false; S.devices = false;
  if (fresh) { S.feed = null; S.body = null; }
  render();
  if (fresh) document.getElementById("panel").scrollTop = 0;
  try {
    const t = await api("GET", `/api/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(id)}`);
    if (gen === S.bodyGen && isSelected(project, id)) { S.body = { project, task: id, text: t.body }; render(); }
  } catch { /* the card still shows the rest */ }
}

// loadFeed reads a session's history and merges it with what the stream
// brought meanwhile (by activity number). A response for a card that is
// no longer open is dropped.
async function loadFeed(project, task, sessionID) {
  if (S.loadingFeed === sessionID) { S.feedAgain = true; return; } // after the one in flight
  S.loadingFeed = sessionID;
  if (!S.feed || S.feed.session !== sessionID) S.feed = { project, task, session: sessionID, bySeq: new Map() };
  try {
    const f = await api("GET", `/api/sessions/${encodeURIComponent(sessionID)}/feed`);
    if (S.feed && S.feed.session === sessionID && isSelected(project, task)) {
      for (const a of f.activities) addActivity(a);
      delete S.feedError[sessionID];
    }
  } catch (e) {
    S.feedError[sessionID] = Date.now(); // retried after a pause, not at once
    if (S.feed && S.feed.session === sessionID && !S.feed.bySeq.size) S.feed = null;
    else S.feedAgain = true; // a populated feed still needs the missed part
    setTimeout(() => { if (isSelected(project, task)) render(); }, 5000);
  }
  S.loadingFeed = null;
  if (S.feedAgain && isSelected(project, task) && !S.feedError[sessionID]) {
    S.feedAgain = false;
    loadFeed(project, task, sessionID);
    return;
  }
  if (isSelected(project, task)) render();
}

function reloadOpenFeed() {
  if (S.feed && isSelected(S.feed.project, S.feed.task)) loadFeed(S.feed.project, S.feed.task, S.feed.session);
}

function addActivity(a) {
  if (!S.feed || !a.seq) return;
  S.feed.bySeq.set(a.seq, a);
}

function renderPanel() {
  const panel = document.getElementById("panel");
  const body = document.getElementById("panel-body");
  const show = (on) => { panel.hidden = !on; document.body.classList.toggle("has-panel", on); };
  if (S.inbox) { show(true); body.replaceChildren(...nodes(inboxView())); return; }
  if (S.devices) { show(true); body.replaceChildren(...nodes(devicesView())); return; }
  if (S.selected && S.selected.newIdea) { show(true); body.replaceChildren(...nodes(newIdeaView(S.selected.project))); return; }
  if (S.selected && S.selected.idea) {
    const ip = S.snap.projects.find(x => x.name === S.selected.project);
    const idea = (S.snap.ideas || []).find(x => x.id === S.selected.task);
    if (!ip || !idea) { show(false); return; }
    show(true);
    const isess = ideaSession(idea);
    if (isess && S.feed && S.feed.session !== isess.id) S.feed = null;
    const recent = isess && S.feedError[isess.id] && Date.now() - S.feedError[isess.id] < 5000;
    if (isess && !recent && (!S.feed || S.feedAgain)) {
      if (S.feedError[isess.id]) delete S.feedError[isess.id];
      S.feedAgain = false;
      loadFeed(ip.name, idea.id, isess.id);
    }
    body.replaceChildren(...nodes(ideaView(ip, idea)));
    renderFeed();
    return;
  }
  const p = S.selected && S.snap.projects.find(x => x.name === S.selected.project);
  const t = p && p.tasks.find(x => x.id === S.selected.task);
  if (!t) { show(false); return; }
  show(true);
  const sess = latestSessions(p.name).get(t.id);
  if (sess && S.feed && S.feed.session !== sess.id) S.feed = null;
  const recentError = sess && S.feedError[sess.id] && Date.now() - S.feedError[sess.id] < 5000;
  if (sess && !recentError && (!S.feed || S.feedAgain)) { // a session started elsewhere, too
    if (S.feedError[sess.id]) delete S.feedError[sess.id];
    S.feedAgain = false;
    loadFeed(p.name, t.id, sess.id);
  }
  body.replaceChildren(...nodes(taskView(p, t, sess)));
  renderFeed();
}

function taskView(p, t, sess) {
  const out = [h("h1", {}, t.id + "  " + t.title)];
  const facts = [["lifecycle", t.lifecycle], ["priority", "P" + t.priority], ["kind", t.kind], ["component", t.component]];
  if (t.depends_on.length) facts.push(["depends on", t.depends_on.join(", ")]);
  facts.push(["touches", t.touches.join(", ")]);
  if (t.merge_commit) facts.push(["merge", t.merge_commit.slice(0, 10)]);
  if (t.state) facts.push(["branch", t.state.branch], ["worktree", t.state.worktree]);
  out.push(h("dl", { class: "facts-list" }, facts.map(([k, v]) => [h("dt", {}, k), h("dd", {}, v)])));
  if (t.lifecycle === "backlog" && !t.ready) {
    out.push(h("h3", {}, "Not ready"), h("ul", { class: "reasons" }, t.reasons.map(r => h("li", {}, r))));
  }
  if (t.lifecycle === "backlog" && t.ready) out.push(runForm(p, t));
  if (sess) out.push(sessionView(sess));
  if (S.body && S.body.project === p.name && S.body.task === t.id) out.push(h("h3", {}, "Task"), h("div", { class: "body" }, S.body.text));
  return out;
}

function runForm(p, t) {
  const route = p.routes[t.id] || {};
  const key = `run:${p.name}/${t.id}`;
  // an invalid route preselects nothing: the person picks a profile
  const select = draft(h("select", { "aria-label": "Profile" },
    h("option", { value: "", selected: !route.profile, disabled: true }, "Choose a profile…"),
    S.snap.profiles.map(pr => h("option", { value: pr.name, selected: pr.name === route.profile, disabled: !pr.runnable },
      pr.name + " (" + pr.harness + ((route.task_model || pr.model) ? ", " + (route.task_model || pr.model) : "") + (pr.api_billing ? ", API key" : "") + (pr.confined ? "" : pr.isolation === "codex" ? ", codex sandbox" : ", unconfined") + ")" + (pr.runnable ? "" : " — not runnable")))), key + ":profile");
  const person = t.needs_person ? draft(h("input", { type: "checkbox" }), key + ":person") : null;
  const err = h("div", { class: "err" });
  const run = h("button", { class: "primary", type: "button", disabled: !select.value, onclick: async () => {
    run.disabled = true; err.textContent = "";
    try {
      // the routed profile is the server's to choose (its current config);
      // a profile is sent only when the person picked one
      const picked = (key + ":profile") in S.drafts || !route.profile;
      const go = async (anyway) => {
        const s = await api("POST", "/api/run", { project: p.name, task: t.id, profile: picked ? select.value : "", person: !!(person && person.checked), anyway });
        forget(key);
        await refresh();
        loadFeed(p.name, t.id, s.id);
      };
      try { await go(false); } catch (e) { if (!offerAnyway(err, e, "Run anyway", go)) throw e; run.disabled = false; }
    } catch (e) { err.textContent = e.message; run.disabled = false; }
  } }, "Run");
  select.addEventListener("change", () => { run.disabled = !select.value; });
  return [h("h3", {}, "Run"),
    route.error ? h("div", { class: "err" }, route.error) : null,
    h("div", { class: "actions" }, select, run,
      person ? h("label", {}, person, " a person starts or permits this run (⚑)") : null),
    err];
}

function sessionView(sess) {
  const err = h("div", { class: "err" });
  const act = (label, fn, cls) => h("button", { type: "button", class: cls, onclick: async (e) => {
    e.target.disabled = true; err.textContent = "";
    try { await fn(); } catch (x) { err.textContent = x.message; }
    e.target.disabled = false;
  } }, label);
  const sid = encodeURIComponent(sess.id);
  const running = !ENDED.has(sess.state);
  const mine = sess.state !== "handedOver"; // handed over: the person's now
  const sayKey = "say:" + sess.id;
  const idea = !!sess.idea; // a grilling: no task, no Release, Restart or Accept
  const say = draft(h("textarea", { placeholder: running ? "An instruction for the agent…" : "An instruction resumes the conversation…" }), sayKey);
  return [h("h3", {}, "Session"),
    h("dl", { class: "facts-list" },
      h("dt", {}, "state"), h("dd", { class: "st-" + sess.state }, stateWord(sess.state) + (sess.error ? " — " + sess.error : "")),
      h("dt", {}, "profile"), h("dd", {}, sess.profile + (sess.model ? " · " + sess.model : "")),
      h("dt", {}, "billing"), h("dd", {}, sess.billing ? billingChip(sess.billing) : "—"),
      h("dt", {}, "sandbox"), h("dd", {}, sess.state === "handedOver"
        ? "taskmoor's last run: " + (sess.confined ? "on" : sess.isolation === "codex" ? "Codex's" : "off") + "; in the terminal, your own agent settings apply"
        : sess.confined ? "on: files, network and credentials confined"
        : sess.isolation === "codex" ? "Codex's: writes in the worktree, no network; reads are not confined"
        : h("span", { class: "chip api" }, "off — the agent's commands run as you")),
      h("dt", {}, "worktree"), h("dd", { class: "mono" }, sess.worktree),
      h("dt", {}, "resume"), h("dd", { class: "mono" }, sess.resume)),
    h("div", { class: "actions" },
      act("Copy cd", () => copy("cd " + shellQuote(sess.worktree))),
      S.remote ? null : act("Open in Finder", () => api("POST", `/api/sessions/${sid}/open`, { with: "finder" })),
      S.remote ? null : act("Open in editor", () => api("POST", `/api/sessions/${sid}/open`, { with: "editor" })),
      mine ? act("Continue in terminal", async () => {
        if (!confirmStep("Stop the agent and continue this conversation in your terminal?")) return;
        const r = await api("POST", `/api/sessions/${sid}/handover`, {});
        await copy(r.resume); banner("Copied: " + r.resume + (idea ? " — commit the tasks there, then Approve the idea here." : " — then `taskmoor review " + sess.task + "` when done."));
      }) : null,
      running ? act("Stop", () => api("POST", `/api/sessions/${sid}/stop`, {}), "danger") : null,
      mine && !idea ? restartControl(sess, act) : null,
      mine && !idea ? act("Release", async () => {
        if (!confirmStep("Release " + sess.task + "? The session stops and the claim goes; the worktree and branch stay.")) return;
        await api("POST", `/api/sessions/${sid}/release`, {});
      }, "danger") : null),
    mine ? h("div", {}, say, h("div", { class: "actions" }, act("Say", async () => {
      const sent = say.value;
      if (!sent.trim()) return;
      const go = (anyway) => api("POST", `/api/sessions/${sid}/say`, { text: sent, anyway });
      try { await go(false); } catch (e) {
        if (!offerAnyway(err, e, "Say anyway", async () => {
          await go(true);
          // clear only what was sent: a newer draft stays
          if ((S.drafts[sayKey] ?? sent) === sent) {
            delete S.drafts[sayKey];
            const cur = document.querySelector(`[data-draft="${CSS.escape(sayKey)}"]`);
            if (cur && cur.value === sent) cur.value = "";
          }
        })) throw e;
        return;
      }
      // clear only what was sent: a newer draft typed meanwhile stays
      if ((S.drafts[sayKey] ?? sent) === sent) {
        delete S.drafts[sayKey];
        const cur = document.querySelector(`[data-draft="${CSS.escape(sayKey)}"]`);
        if (cur && cur.value === sent) cur.value = "";
      }
    }, "primary"))) : h("p", { class: "empty" }, idea ? "This grilling continues in your terminal; commit the tasks there, then Approve the idea." : "This session continues in your terminal; finish with `taskmoor review " + sess.task + "`."),
    err,
    idea ? null : acceptanceView(sess),
    h("h3", {}, "Feed"),
    h("ul", { class: "feed", id: "feed", "data-session": sess.id })];
}

// acceptanceView is the review and Accept (A5, A8): loaded on request,
// the diff can be large.
function acceptanceView(sess) {
  if (sess.state === "accepted") {
    const rvLine = sess.review ? " Review: " + sess.review.done + " of " + sess.review.rounds + " round(s), " + reviewState(sess.review, sess) + "." : "";
    return h("p", {}, "Accepted: merged as " + (sess.merged || "").slice(0, 12) + "." + rvLine + " Push is yours.");
  }
  const rv = sess.review;
  const box = h("div", { class: "acceptance" });
  const head = [h("h3", {}, "Acceptance")];
  if (rv) head.push(h("p", {}, "Review: " + rv.done + " of " + rv.rounds + " round(s), " + reviewState(rv) + (rv.partial ? " (partial: " + rv.partial + ")" : "") +
    (rv.findings ? " (" + rv.findings + " finding(s))" : "") + (rv.unreviewed ? "; the last fixes are not reviewed again" : "") + (rv.error ? " — " + rv.error : "")));
  const err = h("div", { class: "err" });
  const load = async () => {
    err.textContent = "";
    try { S.accept[sess.id] = await api("GET", `/api/sessions/${encodeURIComponent(sess.id)}/acceptance`); render(); }
    catch (e) { err.textContent = e.message; }
  };
  const a = S.accept[sess.id];
  if (!a) { box.replaceChildren(...head, h("div", { class: "actions" }, h("button", { type: "button", onclick: load }, "Show the work to accept")), err); return box; }
  const sendKey = "sendback:" + sess.id;
  const note = draft(h("textarea", { placeholder: "What to change (sent back to the session)…" }), sendKey);
  box.replaceChildren(...[...head,
    h("p", {}, a.ready ? "Ready to accept." : "Not ready: " + a.why),
    h("div", { class: "desc" }, (a.commits || []).length + " commit(s) on " + a.session.branch + " since " + a.base.slice(0, 10) + ", up to " + (a.tip || "").slice(0, 10)),
    h("ul", {}, (a.commits || []).map(c => h("li", { class: "mono" }, c))),
    h("pre", { class: "mono body" }, a.stat || "(no changes)"),
    a.dirty ? h("p", { class: "err" }, "Uncommitted in the worktree (not merged): " + a.dirty) : null,
    a.checks ? [h("h3", {}, "Checks"), h("div", { class: "body" }, a.checks)] : null,
    a.result ? [h("h3", {}, "The agent's summary"), h("div", { class: "body" }, a.result)] : null,
    rv && rv.report ? h("details", {}, h("summary", {}, "Review findings"), h("pre", { class: "body" }, rv.report)) : null,
    h("details", {}, h("summary", {}, "Diff" + (a.diff_cut ? " (cut)" : "")), h("pre", { class: "mono body diff" }, a.diff)),
    h("div", { class: "actions" },
      h("button", { type: "button", class: "primary", disabled: !a.ready, onclick: async () => {
        if (!confirmStep("Merge " + sess.branch + " at " + (a.tip || "").slice(0, 10) + " into " + a.main_branch + "? A trial merge runs first; push stays yours.")) return;
        err.textContent = "";
        try {
          // exactly the commit shown: a branch that moved since is refused
          const r = await api("POST", `/api/sessions/${encodeURIComponent(sess.id)}/accept`, { tip: a.tip });
          delete S.accept[sess.id];
          banner(r.conflicted ? "Conflicts with " + a.main_branch + ((r.conflicts || []).length ? " in " + r.conflicts.join(", ") : "") + ": sent back to the session." : "Merged as " + (r.merged || "").slice(0, 12) + ". Push is yours.");
          await refresh();
        } catch (e) { err.textContent = e.message; }
      } }, "Accept"),
      h("button", { type: "button", onclick: load }, "Refresh")),
    note,
    h("div", { class: "actions" }, h("button", { type: "button", onclick: async () => {
      if (!note.value.trim()) return;
      const sentNote = note.value;
      const text = "Sent back from acceptance: " + sentNote;
      const go = async (anyway) => {
        await api("POST", `/api/sessions/${encodeURIComponent(sess.id)}/say`, { text, anyway });
        if ((S.drafts[sendKey] ?? sentNote) === sentNote) forget(sendKey); // a newer draft stays
        delete S.accept[sess.id];
        await refresh();
      };
      try { await go(false); }
      catch (e) { if (!offerAnyway(err, e, "Send back anyway", go)) err.textContent = e.message; }
    } }, "Send back")),
    err].flat(Infinity).filter(x => x != null)); // replaceChildren takes nodes only
  return box;
}

// offerAnyway turns a refusal of the usage guard (the plan below the
// threshold) into a button that starts it anyway; false for other errors.
function offerAnyway(errBox, e, label, go) {
  if (!/--anyway/.test(e.message)) return false;
  errBox.replaceChildren(e.message.replace(/ with --anyway.*$/, "."), " ",
    h("button", { type: "button", class: "danger", onclick: async () => {
      try { await go(true); errBox.replaceChildren(); } catch (x) { errBox.textContent = x.message; }
    } }, label));
  return true;
}

// renderUsage shows each plan's share left and the Mac held awake (A19).
function renderUsage() {
  const box = document.getElementById("usage");
  const th = S.snap.threshold || 0;
  const parts = (S.snap.usage || []).map(u => {
    const low = u.left >= 0 && th > 0 && u.left < th;
    const title = (u.windows || []).map(w => w.name + " " + w.left + "%" + (w.resets_at ? " (resets " + w.resets_at + ")" : "")).join(", ") + (u.error ? " — last read failed: " + u.error : "");
    return h("span", { class: "chip" + (low ? " api" : ""), title }, u.harness + " " + (u.left >= 0 ? u.left + "%" : "?"));
  });
  if (S.snap.agents_running) parts.push(h("span", { class: "awake", title: "the service holds macOS's idle-sleep assertion" }, "agents running — keep the Mac awake (lid open or on power)"));
  if (S.snap.awake_error) parts.push(h("span", { class: "err" }, S.snap.awake_error));
  if (S.remote) {
    parts.push(h("button", { type: "button", class: "chip", title: "sign out of the VPS (this device stays paired)", onclick: signOutOutside }, "sign out"));
  }
  if (!S.remote && S.snap.remote_host) {
    parts.push(h("button", { type: "button", class: "chip", title: "devices that reach the board from https://" + S.snap.remote_host,
      onclick: () => { S.inbox = false; S.selected = null; S.devices = true; render(); loadDevices(); } }, "devices"));
  }
  const tg = S.snap.telegram;
  if (tg && tg.configured) parts.push(h("span", { class: "chip" + (tg.paired && !tg.error ? "" : " api"),
    title: tg.error || (tg.paired ? "questions go to Telegram" : "not paired: taskmoor telegram pair") }, "telegram " + (tg.paired ? "✓" : "?")));
  box.replaceChildren(...parts);
}

// isolationChip marks a session whose commands are not in Claude's sandbox.
function isolationChip(iso) {
  return iso === "codex"
    ? h("span", { class: "chip", title: "Codex's sandbox: writes in the worktree, no network; reads are not confined" }, "codex sandbox")
    : h("span", { class: "chip api", title: "the agent's commands run without a sandbox" }, "unconfined");
}

// restartControl is "Restart with a profile" (A24): the same claim,
// worktree and branch, a new session of the chosen profile.
function restartControl(sess, act) {
  const key = "restart:" + sess.id;
  const sel = draft(h("select", { "aria-label": "Restart with profile" },
    S.snap.profiles.filter(p => p.runnable).map(p => h("option", { value: p.name, selected: p.name === sess.profile },
      p.name + " (" + p.harness + (p.model ? ", " + p.model : "") + ")"))), key);
  const rerr = h("span", { class: "err" });
  return h("span", { class: "inline" }, sel, act("Restart with", async () => {
    if (!confirmStep("Stop this session and start " + sel.value + " on " + sess.task + " in the same worktree? Its work stays.")) return;
    const chosen = sel.value; // the profile confirmed, whatever the select shows later
    const go = async (anyway) => {
      const r = await api("POST", `/api/sessions/${encodeURIComponent(sess.id)}/restart`, { profile: chosen, anyway });
      forget(key);
      if (r.error) throw new Error(r.error);
    };
    rerr.replaceChildren();
    try { await go(false); } catch (e) { if (!offerAnyway(rerr, e, "Restart " + chosen + " anyway", go)) throw e; }
  }), rerr);
}

// renderKeep is the project's "Keep N busy" (A4) in the top bar.
function renderKeep(p) {
  const box = document.getElementById("keep");
  if (!p) { box.replaceChildren(); return; }
  const key = "keep:" + p.name;
  const n = draft(h("input", { type: "number", min: "0", max: "32", value: String(p.keep_busy || 0), "aria-label": "Sessions to keep busy", class: "num" }), key);
  const err = h("span", { class: "err" }, p.keep_error || "");
  box.replaceChildren(h("label", { title: "The service starts ready tasks (priority then id, never needs_person) until this many sessions are under way" }, "Keep busy ", n), " ",
    h("button", { type: "button", onclick: async () => {
      try { await api("POST", `/api/projects/${encodeURIComponent(p.name)}/keep`, { n: Number(n.value) }); forget(key); await refresh(); }
      catch (e) { err.textContent = e.message; }
    } }, "Set"), " ", err);
}

// confirmStep is a plain confirmation the person gives in the page.
function confirmStep(text) { return window.confirm(text); }

function renderFeed() {
  const ul = document.getElementById("feed");
  if (!ul) return;
  // only the feed of the session this card shows
  const acts = S.feed && S.feed.session === ul.dataset.session
    ? [...S.feed.bySeq.values()].sort((a, b) => a.seq - b.seq) : [];
  const atBottom = ul.scrollHeight - ul.scrollTop - ul.clientHeight < 40;
  ul.replaceChildren(...acts.map(a => h("li", { class: a.type },
    h("span", { class: "t" }, clock(a.at)),
    h("span", { class: "k" }, a.type + (a.tool ? " " + a.tool : "") + (a.by && a.by !== "person" ? " (" + a.by + ")" : "")),
    h("span", { class: "x" }, a.text || ""))));
  if (atBottom) ul.scrollTop = ul.scrollHeight; // follow the newest unless the person scrolled up
}

function clock(at) {
  const d = new Date(at);
  return isNaN(d) ? at : d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

function inboxView() {
  const asks = S.snap.asks;
  const out = [h("h1", {}, "Needs you")];
  if (S.remote) out.push(pushControl());
  if (!asks.length) out.push(h("p", { class: "empty" }, "Nothing waits for you."));
  for (const a of asks) out.push(askForm(a));
  // what the dispatcher answered (C3): the person can override (a)/(b)
  const ds = (S.snap.decisions || []).filter(d => d.answered_by === "dispatcher");
  out.push(h("h3", {}, "Answered by the dispatcher"));
  if (!S.snap.dispatcher) out.push(h("p", { class: "empty" }, "The dispatcher is off (dispatcher_profile in the config): every question comes here."));
  else if (!ds.length) out.push(h("p", { class: "empty" }, "Nothing yet."));
  for (const d of ds) out.push(decisionView(d));
  return out;
}

function decisionView(d) {
  const err = h("div", { class: "err" });
  const q = d.questions.find(x => !(x.free_text && x.optional)) || { question: "", options: [] };
  const answer = (d.answer || []).filter(a => a && a.length).map(a => a.join(", ")).join("; ");
  const parts = [h("div", { class: "row" }, h("b", {}, d.project + " · " + d.task), " ", h("span", { class: "chip" }, "level " + d.level), " ", h("span", { class: "t" }, clock(d.at))),
    h("div", { class: "q" }, d.questions.filter(x => !(x.free_text && x.optional)).map(x => x.question).join(" / ")),
    h("div", {}, "→ " + answer),
    d.citation ? h("div", { class: "desc" }, d.citation) : null,
    h("div", { class: "desc" }, d.reason)];
  if (d.overridden) parts.push(h("div", {}, "overridden: " + d.overridden.answer + " (sent " + clock(d.overridden.at) + ")"));
  if (d.questions.filter(x => !(x.free_text && x.optional)).length === 1) {
    const key = "override:" + d.id;
    // by number: a label can itself be a number
    const sel = draft(h("select", { "aria-label": "Your answer" }, (q.options || []).map((o, i) => h("option", { value: String(i + 1) }, o.label))), key);
    parts.push(h("div", { class: "actions" }, sel, h("button", { type: "button", onclick: async () => {
      if (!confirmStep("Send your answer to the session? The agent may already have acted on the dispatcher's.")) return;
      try { await api("POST", `/api/decisions/${encodeURIComponent(d.id)}/override`, { choices: [[sel.value]] }); forget(key); await refresh(); }
      catch (e) { err.textContent = e.message; }
    } }, "Override")));
  } else {
    parts.push(h("div", { class: "desc" }, "override: taskmoor override " + d.id + " N …"));
  }
  parts.push(err);
  return h("div", { class: "ask decided" }, parts);
}

function askForm(a) {
  const err = h("div", { class: "err" });
  const head = h("div", { class: "row" }, h("b", {}, a.project + " · " + a.task), " ", h("span", { class: "chip" }, a.kind), " ",
    (S.snap.ideas || []).some(i => i.id === a.task && i.project === a.project)
      ? h("a", { href: "#", onclick: (e) => { e.preventDefault(); openIdea(a.project, a.task); } }, "open the idea")
      : h("a", { href: "#", onclick: (e) => { e.preventDefault(); openTask(a.project, a.task); } }, "open the task"),
    a.dispatch ? h("div", { class: "desc" }, "dispatcher: " + a.dispatch) : null);
  const send = async (body) => {
    err.textContent = "";
    try { await api("POST", `/api/asks/${encodeURIComponent(a.id)}/answer`, body); forget("ask:" + a.id); await refresh(); }
    catch (e) { err.textContent = e.message; }
  };
  if (a.kind === "approval") {
    const note = draft(h("input", { type: "text", placeholder: "Why (optional, sent with a deny)" }), `ask:${a.id}:note`);
    return h("div", { class: "ask" }, head,
      h("div", { class: "q" }, a.tool + " — " + a.reason),
      h("pre", { class: "mono" }, a.detail || ""),
      h("div", { class: "actions" },
        h("button", { class: "primary", type: "button", onclick: () => send({ allow: true }) }, "Allow"),
        h("button", { class: "danger", type: "button", onclick: () => send({ allow: false, text: note.value }) }, "Deny"),
        note),
      err);
  }
  const groups = a.questions.map((q, qi) => {
    q.options = q.options || [];
    // the recommended option first (F9)
    const order = q.options.map((o, i) => i);
    if (q.recommended >= 0) { order.splice(order.indexOf(q.recommended), 1); order.unshift(q.recommended); }
    const name = `q-${a.id}-${qi}`;
    if (q.free_text) { // a typed answer of its own (ACP forms)
      return h("fieldset", { class: "plain" },
        h("div", { class: "q" }, (q.header ? "[" + q.header + "] " : "") + q.question),
        draft(h("input", { type: q.secret ? "password" : "text", name, "data-free": "1", placeholder: q.optional ? "optional" : "your answer" }), `ask:${a.id}:${qi}:text`));
    }
    return h("fieldset", { class: "plain" },
      h("div", { class: "q" }, (q.header ? "[" + q.header + "] " : "") + q.question),
      order.map(i => {
        const o = q.options[i];
        const input = draft(h("input", { type: q.multi_select ? "checkbox" : "radio", name, value: String(i + 1), checked: i === q.recommended && !q.multi_select }), `ask:${a.id}:${qi}:${i}`);
        return h("div", {},
          h("label", {}, input, " " + o.label, i === q.recommended ? h("span", { class: "rec" }, "recommended") : null),
          o.description ? h("div", { class: "desc" }, o.description) : null,
          o.preview ? h("pre", { class: "preview" }, o.preview) : null);
      }));
  });
  const other = draft(h("input", { type: "text", placeholder: "Or answer in your own words (first question)" }), `ask:${a.id}:other`);
  const form = h("div", { class: "ask" }, head, groups,
    h("div", { class: "actions" }, other,
      h("button", { class: "primary", type: "button", onclick: () => {
        if (other.value.trim()) return send({ text: other.value.trim() });
        const choices = a.questions.map((q, qi) => q.free_text
          ? [form.querySelector(`input[name="q-${a.id}-${qi}"]`).value].filter(v => v.trim())
          : [...form.querySelectorAll(`input[name="q-${a.id}-${qi}"]:checked`)].map(i => i.value));
        if (choices.some((c, qi) => !c.length && !a.questions[qi].optional)) { err.textContent = "Answer every question that is not optional."; return; }
        return send({ choices });
      } }, "Answer")),
    err);
  return form;
}

async function copy(text) {
  try { await navigator.clipboard.writeText(text); } catch { window.prompt("Copy:", text); }
}

function shellQuote(p) { return /^[\w./-]+$/.test(p) ? p : "'" + p.replace(/'/g, "'\\''") + "'"; }

// ---- wiring ----

document.getElementById("project").addEventListener("change", (e) => {
  S.project = e.target.value; localSet("project", S.project);
  S.selected = null; S.feed = null; S.body = null; render();
});
document.getElementById("inbox-btn").addEventListener("click", () => { S.inbox = true; S.devices = false; S.selected = null; render(); });
const closePanel = () => { S.inbox = false; S.devices = false; S.selected = null; S.feed = null; S.body = null; render(); };
document.getElementById("panel-close").addEventListener("click", closePanel);
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closePanel(); });
// the board as an app (stage 10): its files for offline, its notifications
if ("serviceWorker" in navigator) navigator.serviceWorker.register("/sw.js").catch(() => { /* not a secure context */ });

// a notification opens the inbox (#inbox)
function inboxFromHash() {
  if (location.hash !== "#inbox") return; // shown only with a snapshot, which a refused credential clears
  history.replaceState(null, "", location.pathname);
  S.inbox = true; S.devices = false; S.selected = null;
  if (S.snap) render();
}
window.addEventListener("hashchange", inboxFromHash);
inboxFromHash();

// the listener says whether this is the Mac's own board or a device's
(async () => {
  // remembered: with no network the board still knows what it is
  try { S.remote = !!(await (await fetch("/api/whoami")).json()).remote; localSet("remote", S.remote ? "1" : "0"); }
  catch { S.remote = localGet("remote") === "1"; }
  if (S.remote) {
    if (localGet("device")) refresh(); else showSignin();
    return;
  }
  window.addEventListener("hashchange", () => { signIn().then(() => { if (localGet("token")) refresh(); }); });
  signIn().then(() => { if (localGet("token")) refresh(); else showSignin(); });
})();
