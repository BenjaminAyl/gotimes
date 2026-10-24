// rss frontend: two panes, no framework. Lists and articles are prefetched into
// memory and shown instantly; the server is asked again in the background.

const $ = (id) => document.getElementById(id);
const tabsEl = $("tabs");
const listEl = $("list");
const viewerEl = $("viewer");
const filterEl = $("filter");

const state = {
  feeds: [],
  totalUnread: 0,
  tab: "unread", // "unread" | "all" | "feeds" | "feed:<id>"
  q: "",
  items: [], // what the list currently shows
  current: 0, // open item id
};

const lists = new Map(); // query string -> items
const articles = new Map(); // item id -> item with content
const pending = new Map(); // in-flight request key -> promise
const MAX_ARTICLES = 200;

// ---------- api ----------

async function api(method, path, body) {
  const res = await fetch("/api" + path, {
    method,
    headers: { "X-Requested-With": "rss", ...(body ? { "Content-Type": "application/json" } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch { /* not json */ }
    throw new Error(msg);
  }
  return res.status === 204 ? null : res.json();
}

// once() shares one request between everyone asking for the same key.
function once(key, run) {
  if (!pending.has(key)) pending.set(key, run().finally(() => pending.delete(key)));
  return pending.get(key);
}

const span = (cls, text) => Object.assign(document.createElement("span"), { className: cls, textContent: text });
const feedOf = (id) => state.feeds.find((f) => f.id === id);
const feedName = (f) => f.title || f.url;

// A feed's icon, or its first letter in a small tile when it has none (or fails to load).
function feedIcon(feedId, name) {
  const f = feedOf(feedId);
  if (!f?.icon_v) return letterIcon(name || (f && feedName(f)));
  const img = document.createElement("img");
  img.className = "fav";
  img.src = `/api/feeds/${feedId}/icon?v=${f.icon_v}`;
  img.alt = "";
  img.decoding = "async";
  img.dataset.name = name || feedName(f);
  return img;
}
function letterIcon(name) {
  const t = span("fav letter", (name || "?").replace(/^(https?:\/\/)?(www\.)?/, "").charAt(0).toUpperCase());
  t.setAttribute("aria-hidden", "true");
  return t;
}
// image errors don't bubble, so catch them while they travel down
document.addEventListener("error", (e) => {
  if (e.target.matches?.("img.fav")) e.target.replaceWith(letterIcon(e.target.dataset.name));
}, true);

function listQuery(tab, q) {
  const p = new URLSearchParams();
  if (tab === "unread") p.set("unread", "1");
  if (tab.startsWith("feed:")) p.set("feed", tab.slice(5));
  if (q) p.set("q", q);
  return p.toString();
}

function fetchList(query) {
  return once("list:" + query, async () => {
    const { items } = await api("GET", "/items?" + query);
    lists.set(query, items);
    return items;
  });
}

function fetchArticle(id) {
  return once("item:" + id, async () => {
    const item = await api("GET", "/items/" + id);
    articles.set(id, item);
    if (articles.size > MAX_ARTICLES) articles.delete(articles.keys().next().value);
    return item;
  });
}

async function loadFeeds() {
  const data = await api("GET", "/feeds");
  state.feeds = data.feeds;
  state.totalUnread = data.total_unread;
  renderTabs();
  if (state.tab === "feeds") renderFeeds();
}

// ---------- prefetch ----------

const idle = (fn, delay) => setTimeout(fn, delay);
const prefetchList = (query) => { if (!lists.has(query)) fetchList(query).catch(() => {}); };
const prefetchArticle = (id) => { if (!articles.has(id)) fetchArticle(id).catch(() => {}); };

function warmLists() {
  const queries = [listQuery("unread", ""), listQuery("all", "")];
  state.feeds.slice(0, 20).forEach((f) => queries.push(listQuery("feed:" + f.id, "")));
  queries.forEach((q, i) => idle(() => prefetchList(q), 150 * i));
}
function warmArticles(items) {
  items.slice(0, 6).forEach((it, i) => idle(() => prefetchArticle(it.id), 250 * (i + 1)));
}

// Intent: start fetching on hover/touch, a beat before the click lands.
document.addEventListener("pointerover", intent);
document.addEventListener("pointerdown", intent);
function intent(e) {
  const row = e.target.closest?.("[data-id]");
  if (row) return prefetchArticle(Number(row.dataset.id));
  const tab = e.target.closest?.("[data-tab]");
  if (tab && tab.dataset.tab !== "feeds") prefetchList(listQuery(tab.dataset.tab, ""));
}

// ---------- tabs ----------

function tabButton(id, label, extra, icon) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "tab";
  b.dataset.tab = id;
  b.setAttribute("role", "tab");
  b.setAttribute("aria-selected", String(state.tab === id));
  if (icon) b.append(icon);
  b.append(label);
  if (extra) b.append(extra);
  return b;
}

function renderTabs() {
  const tabs = [];
  tabs.push(tabButton("unread", "Unread", state.totalUnread ? span("n", state.totalUnread) : null));
  tabs.push(tabButton("all", "All"));
  const pinned = state.feeds.filter((f) => f.pinned);
  for (const f of pinned) tabs.push(tabButton("feed:" + f.id, feedName(f), f.unread ? span("n", f.unread) : null, feedIcon(f.id)));
  // a feed opened from the Feeds tab gets a temporary tab so you can close it again
  if (state.tab.startsWith("feed:")) {
    const f = feedOf(Number(state.tab.slice(5)));
    if (f && !f.pinned) {
      const x = span("x", "×");
      x.dataset.close = "1";
      tabs.push(tabButton(state.tab, feedName(f), x, feedIcon(f.id)));
    }
  }
  tabs.push(tabButton("feeds", "Feeds"));
  tabsEl.replaceChildren(...tabs);
  filterEl.placeholder = state.tab === "feeds" ? "Filter feeds" : "Filter";
}

function setTab(tab) {
  state.tab = tab;
  filterEl.value = "";
  state.q = "";
  document.body.dataset.view = "list";
  renderTabs();
  show();
}

tabsEl.addEventListener("click", (e) => {
  const tab = e.target.closest("[data-tab]");
  if (!tab) return;
  if (e.target.dataset.close) return setTab("unread");
  setTab(tab.dataset.tab);
});

// ---------- list ----------

function rowFor(it) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "row";
  b.dataset.id = it.id;
  b.dataset.read = String(it.read);
  if (it.id === state.current) b.setAttribute("aria-current", "true");
  const meta = document.createElement("span");
  meta.className = "meta";
  meta.append(feedIcon(it.feed_id, it.feed_title), span("", [it.feed_title, ago(it.published), it.author].filter(Boolean).join(" · ")));
  b.append(span("title", it.title || "Untitled"), meta);
  return b;
}

function ago(iso) {
  const mins = Math.max(0, (Date.now() - new Date(iso)) / 60000);
  if (mins < 60) return Math.max(1, Math.round(mins)) + "m";
  if (mins < 1440) return Math.round(mins / 60) + "h";
  if (mins < 10080) return Math.round(mins / 1440) + "d";
  return new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

const signature = (items) => items.map((i) => i.id + (i.read ? "r" : "u")).join();

function renderItems(items) {
  if (state.tab === "feeds") return;
  state.items = items;
  if (!items.length) {
    listEl.replaceChildren(span("note", state.q ? "Nothing matches." : state.tab === "unread" ? "All read." : "Nothing here."));
    return;
  }
  listEl.replaceChildren(...items.map(rowFor));
}

const matches = (it, q) =>
  [it.title, it.summary, it.author, it.feed_title].some((s) => s?.toLowerCase().includes(q));

// show() paints the cached list at once (filtering it locally while a search
// is pending), then swaps in the server's answer if it differs.
function show() {
  if (state.tab === "feeds") return renderFeeds();
  const tab = state.tab;
  const q = state.q;
  const query = listQuery(tab, q);
  const base = lists.get(listQuery(tab, ""));
  const hit = lists.get(query) ?? (q && base ? base.filter((it) => matches(it, q.toLowerCase())) : null);
  if (hit) renderItems(hit);
  else listEl.replaceChildren(span("note", "…"));

  fetchList(query)
    .then((items) => {
      if (tab !== state.tab || q !== state.q) return;
      if (!hit || signature(items) !== signature(hit)) renderItems(items);
      warmArticles(items);
    })
    .catch((err) => {
      if (!hit && tab === state.tab) listEl.replaceChildren(span("note err", err.message));
    });
}

listEl.addEventListener("click", (e) => {
  const row = e.target.closest("[data-id]");
  if (row) openItem(Number(row.dataset.id));
});

let filterTimer;
filterEl.addEventListener("input", () => {
  if (state.tab === "feeds") return renderFeeds();
  const q = filterEl.value.trim();
  state.q = q;
  const base = lists.get(listQuery(state.tab, ""));
  if (base) renderItems(q ? base.filter((it) => matches(it, q.toLowerCase())) : base);
  clearTimeout(filterTimer);
  filterTimer = setTimeout(show, 250);
});
filterEl.addEventListener("keydown", (e) => {
  if (e.key === "Escape") { filterEl.value = ""; filterEl.dispatchEvent(new Event("input")); filterEl.blur(); }
});

// ---------- feeds tab ----------

const SVG = {
  pin: "M9 4.5h6l-.8 5.5 2.8 3H7l2.8-3zM12 13v7",
  check: "M5 12.5l4.5 4.5L19 7",
  x: "M6 6l12 12M18 6L6 18",
};
function iconButton(name, label, action, pressed) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "icon-btn";
  b.dataset.action = action;
  b.title = label;
  b.setAttribute("aria-label", label);
  if (pressed !== undefined) b.setAttribute("aria-pressed", String(pressed));
  b.innerHTML = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="${SVG[name]}"/></svg>`;
  return b;
}

function renderFeeds() {
  const q = filterEl.value.trim().toLowerCase();
  const rows = state.feeds
    .filter((f) => !q || feedName(f).toLowerCase().includes(q))
    .map((f) => {
      const row = document.createElement("div");
      row.className = "feed-row";
      row.dataset.feed = f.id;
      const open = document.createElement("button");
      open.type = "button";
      open.className = "feed-open";
      open.dataset.action = "open";
      open.append(feedIcon(f.id), span("name", feedName(f)));
      if (f.last_error) open.append(Object.assign(span("bad", "!"), { title: f.last_error }));
      if (f.pinned) open.append(span("pin", "●"));
      if (f.unread) open.append(span("n", f.unread));
      const tools = span("feed-tools", "");
      tools.append(
        iconButton("pin", f.pinned ? "Unpin" : "Pin to tabs", "pin", f.pinned),
        iconButton("check", "Mark all read", "read"),
        iconButton("x", "Remove feed", "remove"),
      );
      row.append(open, tools);
      return row;
    });
  listEl.replaceChildren(...(rows.length ? rows : [span("note", state.feeds.length ? "Nothing matches." : "No feeds yet. Press + to add one.")]));
}

async function afterFeedChange() {
  lists.clear();
  await loadFeeds();
  warmLists();
}

// Keep what's on screen, but ask the server again for every list we hold.
async function revalidateAll() {
  await loadFeeds();
  [...lists.keys()].forEach((q, i) => idle(() => fetchList(q).catch(() => {}), 100 * i));
  show();
}

listEl.addEventListener("click", async (e) => {
  const btn = e.target.closest("[data-action]");
  const row = e.target.closest("[data-feed]");
  if (!btn || !row) return;
  const id = Number(row.dataset.feed);
  const f = feedOf(id);
  try {
    switch (btn.dataset.action) {
      case "open": return setTab("feed:" + id);
      case "pin": await api("POST", `/feeds/${id}/pin`); break;
      case "read": await api("POST", `/feeds/${id}/read`); break;
      case "remove":
        if (!confirm(`Remove ${feedName(f)}?`)) return;
        await api("DELETE", `/feeds/${id}`);
        break;
    }
    await afterFeedChange();
    renderTabs();
    renderFeeds();
  } catch (err) {
    alert(err.message);
  }
});

// ---------- viewer ----------

function articleEl(it) {
  const art = document.createElement("article");
  art.className = "article";

  const back = document.createElement("button");
  back.type = "button";
  back.className = "back";
  back.dataset.back = "1";
  back.textContent = "← Back";

  const h1 = document.createElement("h1");
  h1.textContent = it.title || "Untitled";
  const when = new Date(it.published).toLocaleString(undefined, { dateStyle: "long", timeStyle: "short" });
  const byline = span("byline", [it.author, when].filter(Boolean).join(" · "));

  const prose = document.createElement("div");
  prose.className = "prose";
  if (it.content) prose.innerHTML = it.content; // sanitized by the server when stored
  else if (it.summary) prose.append(Object.assign(document.createElement("p"), { textContent: it.summary }));

  const kicker = span("kicker", "");
  kicker.append(feedIcon(it.feed_id, it.feed_title), it.feed_title);
  art.append(back, kicker, h1, byline, prose);
  if (it.url) {
    const a = Object.assign(document.createElement("a"), { href: it.url, target: "_blank", rel: "noopener noreferrer", textContent: "Read the original ↗" });
    const p = span("source", "");
    p.append(a);
    art.append(p);
  }
  return art;
}

async function openItem(id) {
  state.current = id;
  document.body.dataset.view = "reader";
  listEl.querySelectorAll('[aria-current="true"]').forEach((r) => r.removeAttribute("aria-current"));
  listEl.querySelector(`[data-id="${id}"]`)?.setAttribute("aria-current", "true");

  const paint = (it) => {
    if (state.current !== id) return;
    viewerEl.replaceChildren(articleEl(it));
    viewerEl.scrollTop = 0;
  };
  const cached = articles.get(id);
  if (cached) paint(cached);
  markRead(id);
  try {
    paint(cached ?? (await fetchArticle(id)));
  } catch (err) {
    if (state.current === id) viewerEl.replaceChildren(span("note err", err.message));
  }
}

// markRead updates every cached copy at once, so lists never go stale.
function markRead(id) {
  const known = [...lists.values()].flat().find((it) => it.id === id) ?? articles.get(id);
  if (known?.read) return;
  for (const items of lists.values()) for (const it of items) if (it.id === id) it.read = true;
  const art = articles.get(id);
  if (art) art.read = true;
  const row = listEl.querySelector(`[data-id="${id}"]`);
  if (row) row.dataset.read = "true";
  const feed = known && feedOf(known.feed_id);
  if (feed?.unread > 0) {
    feed.unread--;
    state.totalUnread = Math.max(0, state.totalUnread - 1);
    renderTabs();
  }
  api("POST", `/items/${id}/read`).catch(() => loadFeeds());
}

viewerEl.addEventListener("click", (e) => {
  if (e.target.closest("[data-back]")) document.body.dataset.view = "list";
});

// ---------- add, refresh ----------

const dialog = $("add");
const addForm = $("add-form");
const addUrl = $("add-url");
const addError = $("add-error");
const addSubmit = $("add-submit");

$("add-open").addEventListener("click", () => {
  addError.textContent = "";
  addForm.reset();
  dialog.showModal();
  addUrl.focus();
});
$("add-cancel").addEventListener("click", () => dialog.close());
dialog.addEventListener("click", (e) => { if (e.target === dialog) dialog.close(); });

addForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  addError.textContent = "";
  addSubmit.disabled = true;
  try {
    await api("POST", "/feeds", { url: addUrl.value.trim() });
    dialog.close();
    await afterFeedChange();
    show();
  } catch (err) {
    addError.textContent = err.message;
  } finally {
    addSubmit.disabled = false;
  }
});

const refreshBtn = $("refresh");
refreshBtn.addEventListener("click", async () => {
  if (refreshBtn.classList.contains("busy")) return;
  refreshBtn.classList.add("busy");
  try {
    await api("POST", "/refresh");
    await revalidateAll();
  } catch (err) {
    alert(err.message);
  } finally {
    refreshBtn.classList.remove("busy");
  }
});

// ---------- keyboard ----------

document.addEventListener("keydown", (e) => {
  if (e.metaKey || e.ctrlKey || e.altKey || e.target.matches("input, textarea")) return;
  if (e.key === "/") { e.preventDefault(); filterEl.focus(); return; }
  if (e.key !== "j" && e.key !== "k") return;
  const i = state.items.findIndex((it) => it.id === state.current);
  const next = state.items[e.key === "j" ? i + 1 : Math.max(0, i - 1)];
  if (!next) return;
  openItem(next.id);
  listEl.querySelector(`[data-id="${next.id}"]`)?.scrollIntoView({ block: "nearest" });
});

// ---------- start ----------

renderTabs();
show();
loadFeeds()
  .then(() => {
    if (state.tab !== "feeds") renderItems(state.items);
    warmLists();
  })
  .catch(() => {});
