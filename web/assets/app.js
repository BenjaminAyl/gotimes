// rss frontend: two panes, no framework. Lists and articles are prefetched into
// memory and shown instantly; the server is asked again in the background.

const $ = (id) => document.getElementById(id);
const tabsEl = $("tabs");
const listEl = $("list");
const viewerEl = $("viewer");
const filterEl = $("filter");
const pinsEl = $("pins");
const ASSET_V = document.querySelector('meta[name="asset-v"]').content;

const state = {
  feeds: [],
  categories: [],
  totalUnread: 0,
  tab: "unread", // "unread" | "all" | "feeds" | "feed:<id>" | "cat:<id>"
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
  if (tab.startsWith("cat:")) p.set("category", tab.slice(4));
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
  state.categories = data.categories;
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
  state.categories.forEach((c) => queries.push(listQuery("cat:" + c.id, "")));
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

function tabButton(id, label, extra) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "tab";
  b.dataset.tab = id;
  b.setAttribute("role", "tab");
  b.setAttribute("aria-selected", String(state.tab === id));
  b.append(label);
  if (extra) b.append(extra);
  return b;
}

const catUnread = (id) => state.feeds.reduce((n, f) => n + (f.category_id === id ? f.unread : 0), 0);
const catOf = (id) => state.categories.find((c) => c.id === id);

// A category's icon (an SVG from /assets/icons, tinted to the text colour), or its first letter.
function catIcon(c) {
  if (!c.icon) return letterIcon(c.name);
  const el = span("cat-icon", "");
  el.style.setProperty("--icon", `url("/assets/icons/${encodeURIComponent(c.icon)}.svg?v=${ASSET_V}")`);
  el.setAttribute("aria-hidden", "true");
  return el;
}

// The main views, plus a row of icons for pinned categories and feeds.
function renderTabs() {
  tabsEl.replaceChildren(
    tabButton("unread", "Unread", state.totalUnread ? span("n", state.totalUnread) : null),
    tabButton("all", "All"),
    tabButton("feeds", "Feeds"),
  );
  $("new-cat").hidden = state.tab !== "feeds";
  renderPins();
}

function pinEntries() {
  const entries = [];
  const add = (tab, label, icon, unread) => entries.push({ tab, label, icon, unread });
  const addCat = (c) => add("cat:" + c.id, c.name, catIcon(c), catUnread(c.id));
  const addFeed = (f) => add("feed:" + f.id, feedName(f), feedIcon(f.id), f.unread);
  state.categories.filter((c) => c.pinned).forEach(addCat);
  state.feeds.filter((f) => f.pinned).forEach(addFeed);
  return entries;
}

function renderPins() {
  const buttons = pinEntries().map((e) => {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "pin";
    b.dataset.tab = e.tab;
    b.title = e.label;
    b.setAttribute("aria-label", e.label);
    b.setAttribute("aria-pressed", String(state.tab === e.tab));
    b.append(e.icon);
    if (e.unread) b.append(span("dot", ""));
    return b;
  });
  pinsEl.replaceChildren(...buttons);
  pinsEl.hidden = buttons.length === 0;
}

function setTab(tab) {
  state.tab = tab;
  filterEl.value = "";
  state.q = "";
  document.body.dataset.view = "list";
  renderTabs();
  show();
}

// Pressing the open icon again goes back to Unread.
document.addEventListener("click", (e) => {
  const tab = e.target.closest("#tabs [data-tab], #pins [data-tab]");
  if (!tab) return;
  const id = tab.dataset.tab;
  setTab(id === state.tab && tab.closest("#pins") ? "unread" : id);
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

// ---------- feeds tab: manage categories and feeds ----------

// Fine-line, sharp-cornered marks to match the engraved category icons.
const SVG = {
  pin: '<path d="M9 3h6M10.2 3l.6 6.6L7.5 13h9l-3.3-3.4.6-6.6M12 13v8"/>',
  check: '<path d="M5.5 13l4 4L20 6.5"/>',
  x: '<path d="M6.5 6.5l11 11M17.5 6.5l-11 11"/>',
  edit: '<path d="M8 6.5h8l-1.2 8L12 20.5l-2.8-6z"/><path d="M12 10v8.5"/><path d="M9.5 3.5h5v3h-5z"/><circle cx="12" cy="8.6" r=".9" fill="currentColor" stroke="none"/>',
  folder: '<path d="M3.5 6.5h6l2 2.5h9v10.5h-17z"/>',
};
function iconButton(name, label, action, pressed) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "icon-btn";
  b.dataset.action = action;
  b.title = label;
  b.setAttribute("aria-label", label);
  if (pressed !== undefined) b.setAttribute("aria-pressed", String(pressed));
  b.innerHTML = `<svg viewBox="0 0 24 24" aria-hidden="true">${SVG[name]}</svg>`;
  return b;
}

function openButton(icon, name, unread, extra = []) {
  const open = document.createElement("button");
  open.type = "button";
  open.className = "feed-open";
  open.dataset.action = "open";
  open.append(icon, span("name", name), ...extra);
  if (unread) open.append(span("n", unread));
  return open;
}

function feedRow(f) {
  const row = document.createElement("div");
  row.className = "feed-row";
  row.dataset.feed = f.id;
  const bad = f.last_error ? [Object.assign(span("bad", "!"), { title: f.last_error })] : [];
  const tools = span("feed-tools", "");
  tools.append(
    iconButton("pin", f.pinned ? "Unpin" : "Pin to the icon row", "pin", f.pinned),
    iconButton("folder", "Move to a category", "move"),
    iconButton("edit", "Rename or change icon", "edit-feed"),
    iconButton("check", "Mark all read", "read"),
    iconButton("x", "Remove feed", "remove"),
  );
  row.append(openButton(feedIcon(f.id), feedName(f), f.unread, bad), tools);
  return row;
}

// Heading for feeds that aren't in any category; same shape as a category heading.
function uncategorizedHead() {
  const head = document.createElement("div");
  head.className = "section-head";
  head.setAttribute("role", "heading");
  const label = span("head-label", "");
  label.append(span("name", "Uncategorized"));
  head.append(label);
  return head;
}

// A category is a heading in the feed list; its tools sit on the heading itself.
function categoryHead(c) {
  const head = document.createElement("div");
  head.className = "section-head cat-head";
  head.dataset.cat = c.id;
  const open = document.createElement("button");
  open.type = "button";
  open.className = "head-open";
  open.dataset.action = "open";
  open.title = `Open ${c.name}`;
  open.append(span("name", c.name));
  if (c.icon) open.append(catIcon(c)); // the icon follows the name, like a trailing mark
  const unread = catUnread(c.id);
  if (unread) open.append(span("n", unread));
  const tools = span("feed-tools", "");
  tools.append(
    iconButton("pin", c.pinned ? "Unpin" : "Pin to the icon row", "pin", c.pinned),
    iconButton("edit", "Edit name and icon", "edit"),
    iconButton("check", "Mark all read", "read"),
    iconButton("x", "Delete category", "remove"),
  );
  head.append(open, tools);
  return head;
}

function renderFeeds() {
  const q = filterEl.value.trim().toLowerCase();
  const feeds = state.feeds.filter((f) => !q || feedName(f).toLowerCase().includes(q));
  const out = [];

  for (const c of state.categories) {
    const members = feeds.filter((f) => f.category_id === c.id);
    if (q && !members.length && !c.name.toLowerCase().includes(q)) continue;
    out.push(categoryHead(c), ...members.map(feedRow));
  }
  const loose = feeds.filter((f) => !catOf(f.category_id));
  if (loose.length) {
    if (state.categories.length) out.push(uncategorizedHead());
    out.push(...loose.map(feedRow));
  }
  if (!out.length) out.push(span("note", state.feeds.length ? "Nothing matches." : "No feeds yet. Press + above to add one."));
  listEl.replaceChildren(...out);
}

// A small pop-up list anchored to a button.
let menuEl = null;
function closeMenu() { menuEl?.remove(); menuEl = null; }
function openMenu(anchor, entries) {
  closeMenu();
  menuEl = document.createElement("div");
  menuEl.className = "menu";
  menuEl.setAttribute("role", "menu");
  for (const e of entries) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "menu-item";
    b.setAttribute("role", "menuitem");
    b.setAttribute("aria-checked", String(!!e.checked));
    b.append(e.icon ?? span("fav", ""), span("name", e.label));
    b.addEventListener("click", () => { closeMenu(); e.pick(); });
    menuEl.append(b);
  }
  document.body.append(menuEl);
  const r = anchor.getBoundingClientRect();
  menuEl.style.left = `${Math.min(r.left, window.innerWidth - menuEl.offsetWidth - 8)}px`;
  menuEl.style.top = `${Math.min(r.bottom + 4, window.innerHeight - menuEl.offsetHeight - 8)}px`;
  menuEl.querySelector("button")?.focus();
}
document.addEventListener("click", (e) => { if (menuEl && !menuEl.contains(e.target) && !e.target.closest('[data-action="move"]')) closeMenu(); });
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeMenu(); });

// ---- feed dialog: rename and change the icon ----

const feedDialog = $("feed-edit");
const feedNameInput = $("feed-name");
const feedPreview = $("feed-icon-preview");
const feedFile = $("feed-icon-file");
const feedError = $("feed-edit-error");
const feedSubmit = $("feed-edit-submit");
const MAX_ICON = 256 * 1024;
let feedEditing = null; // { id, file: Blob | null, reset: boolean }

function openFeedDialog(f) {
  feedEditing = { id: f.id, file: null, reset: false };
  feedNameInput.value = f.title === f.site_title ? "" : f.title; // empty means "use the feed's own name"
  feedNameInput.placeholder = f.site_title || f.url;
  feedError.textContent = "";
  feedFile.value = "";
  feedPreview.replaceChildren(feedIcon(f.id));
  $("feed-icon-reset").hidden = !f.custom_icon;
  feedDialog.showModal();
  feedNameInput.focus();
}

// Rasters are shrunk to a small square-ish PNG; SVGs are sent as they are.
async function prepareIcon(file) {
  if (file.type === "image/svg+xml" || file.name.endsWith(".svg")) {
    return new Blob([await file.arrayBuffer()], { type: "image/svg+xml" });
  }
  try {
    const bmp = await createImageBitmap(file);
    const scale = Math.min(1, 96 / Math.max(bmp.width, bmp.height));
    const canvas = document.createElement("canvas");
    canvas.width = Math.max(1, Math.round(bmp.width * scale));
    canvas.height = Math.max(1, Math.round(bmp.height * scale));
    canvas.getContext("2d").drawImage(bmp, 0, 0, canvas.width, canvas.height);
    return await new Promise((resolve, reject) => canvas.toBlob((b) => (b ? resolve(b) : reject(new Error("couldn't read that image"))), "image/png"));
  } catch {
    return file; // e.g. .ico, which the browser can't draw to a canvas: send as is
  }
}

$("feed-icon-pick").addEventListener("click", () => feedFile.click());
feedFile.addEventListener("change", async () => {
  const file = feedFile.files[0];
  if (!file) return;
  feedError.textContent = "";
  const blob = await prepareIcon(file);
  if (blob.size > MAX_ICON) {
    feedFile.value = "";
    feedError.textContent = "That image is too large (256KB max).";
    return;
  }
  feedEditing.file = blob;
  feedEditing.reset = false;
  const img = document.createElement("img");
  img.className = "fav";
  img.alt = "";
  img.dataset.name = feedNameInput.value || feedNameInput.placeholder;
  img.src = URL.createObjectURL(blob);
  feedPreview.replaceChildren(img);
});
$("feed-icon-reset").addEventListener("click", () => {
  feedEditing.file = null;
  feedEditing.reset = true;
  feedFile.value = "";
  $("feed-icon-reset").hidden = true;
  feedPreview.replaceChildren(letterIcon(feedNameInput.value || feedNameInput.placeholder));
});
$("feed-edit-cancel").addEventListener("click", () => feedDialog.close());
feedDialog.addEventListener("click", (e) => { if (e.target === feedDialog) feedDialog.close(); });

$("feed-edit-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  feedError.textContent = "";
  feedSubmit.disabled = true;
  const { id, file, reset } = feedEditing;
  try {
    await api("PATCH", `/feeds/${id}`, { title: feedNameInput.value.trim() });
    if (file) {
      const res = await fetch(`/api/feeds/${id}/icon`, { method: "PUT", headers: { "X-Requested-With": "rss", "Content-Type": file.type || "image/png" }, body: file });
      if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
    } else if (reset) {
      await api("DELETE", `/feeds/${id}/icon`);
    }
    feedDialog.close();
    articles.clear(); // cached articles carry the old feed name
    await afterFeedChange();
    renderTabs();
    if (state.tab === "feeds") renderFeeds();
    else show();
  } catch (err) {
    feedError.textContent = err.message;
  } finally {
    feedSubmit.disabled = false;
  }
});

// ---- category dialog ----

const catDialog = $("cat");
const catForm = $("cat-form");
const catName = $("cat-name");
const catIcons = $("cat-icons");
const catError = $("cat-error");
const catSubmit = $("cat-submit");
let catEditing = 0;
let catChoice = "";
let iconNames = null;

async function openCategoryDialog(cat) {
  catEditing = cat?.id ?? 0;
  catChoice = cat?.icon ?? "";
  catName.value = cat?.name ?? "";
  catError.textContent = "";
  $("cat-title").textContent = cat ? "Edit category" : "New category";
  iconNames ??= api("GET", "/icons").then((d) => d.icons);
  let names = [];
  try { names = await iconNames; } catch (err) { catError.textContent = err.message; iconNames = null; }
  paintIconChoices(names);
  catDialog.showModal();
  catName.focus();
}

function paintIconChoices(names) {
  catIcons.replaceChildren(...names.map((n) => {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "icon-choice";
    b.dataset.icon = n;
    b.title = n;
    b.setAttribute("aria-label", n);
    b.setAttribute("aria-pressed", String(n === catChoice));
    b.append(catIcon({ icon: n, name: n }));
    return b;
  }));
}

$("new-cat").addEventListener("click", () => openCategoryDialog());

catIcons.addEventListener("click", (e) => {
  const b = e.target.closest("[data-icon]");
  if (!b) return;
  catChoice = catChoice === b.dataset.icon ? "" : b.dataset.icon; // pressing again clears it
  catIcons.querySelectorAll("[data-icon]").forEach((x) => x.setAttribute("aria-pressed", String(x.dataset.icon === catChoice)));
});
$("cat-cancel").addEventListener("click", () => catDialog.close());
catDialog.addEventListener("click", (e) => { if (e.target === catDialog) catDialog.close(); });

catForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  catError.textContent = "";
  catSubmit.disabled = true;
  try {
    const body = { name: catName.value.trim(), icon: catChoice };
    if (catEditing) await api("PATCH", `/categories/${catEditing}`, body);
    else await api("POST", "/categories", body);
    catDialog.close();
    await afterFeedChange();
    renderTabs();
    if (state.tab === "feeds") renderFeeds();
  } catch (err) {
    catError.textContent = err.message;
  } finally {
    catSubmit.disabled = false;
  }
});

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
  if (!btn) return;
  const action = btn.dataset.action;
  const feedRowEl = e.target.closest("[data-feed]");
  const catRowEl = e.target.closest("[data-cat]");
  const kind = catRowEl ? "categories" : "feeds";
  const id = Number((catRowEl ?? feedRowEl)?.dataset[catRowEl ? "cat" : "feed"]);
  if (!id) return;
  const f = feedOf(id);
  const c = catOf(id);
  try {
    switch (action) {
      case "open": return setTab((catRowEl ? "cat:" : "feed:") + id);
      case "edit": return openCategoryDialog(c);
      case "edit-feed": return openFeedDialog(f);
      case "move":
        return openMenu(btn, [
          { label: "No category", checked: !f.category_id, pick: () => moveFeed(id, 0) },
          ...state.categories.map((cat) => ({ label: cat.name, icon: catIcon(cat), checked: f.category_id === cat.id, pick: () => moveFeed(id, cat.id) })),
        ]);
      case "pin": await api("POST", `/${kind}/${id}/pin`); break;
      case "read": await api("POST", `/${kind}/${id}/read`); break;
      case "remove":
        if (!confirm(catRowEl ? `Delete the category ${c.name}? Its feeds stay, uncategorized.` : `Remove ${feedName(f)}?`)) return;
        await api("DELETE", `/${kind}/${id}`);
        if (state.tab === (catRowEl ? "cat:" : "feed:") + id) state.tab = "feeds";
        break;
    }
    await afterFeedChange();
    renderTabs();
    renderFeeds();
  } catch (err) {
    alert(err.message);
  }
});

async function moveFeed(feedId, categoryId) {
  try {
    await api("PUT", `/feeds/${feedId}/category`, { category_id: categoryId });
    await afterFeedChange();
    renderTabs();
    renderFeeds();
  } catch (err) {
    alert(err.message);
  }
}

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
