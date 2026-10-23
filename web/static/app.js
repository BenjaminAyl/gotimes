// Small behaviours htmx doesn't cover: the add dialog, sidebar filter, both
// searches, and the list/reader switch on narrow screens. No inline scripts
// (see the CSP).
(() => {
  const $ = (id) => document.getElementById(id);
  const dialog = $("add-dialog");
  const form = $("add-form");
  const notice = $("add-notice");
  const view = $("view").elements; // feed, unread, q, story
  const reader = $("reader");
  const items = $("items");
  const masthead = $("masthead");
  const storySearch = $("story-search");
  const storyInput = storySearch.elements.q;
  const secFilter = $("sec-filter");
  let resetItemScroll = false;

  const showPane = (name) => { document.body.dataset.view = name; };
  const reloadItems = () => {
    showPane("list");
    resetItemScroll = true;
    htmx.trigger(document.body, "filter-changed");
  };

  function setFilter(feed, unread) {
    view.feed.value = feed;
    view.unread.value = unread;
    reloadItems();
  }

  // ---- search dispatches (server side, debounced) ----
  let timer;
  storyInput.addEventListener("input", () => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      view.q.value = storyInput.value.trim();
      reloadItems();
    }, 250);
  });
  storySearch.addEventListener("submit", (e) => e.preventDefault());

  function toggleStorySearch(open) {
    const toggle = document.querySelector("[data-search-toggle]");
    storySearch.hidden = !open;
    toggle.setAttribute("aria-expanded", String(open));
    if (open) {
      masthead.dataset.searching = "";
      storyInput.focus();
    } else {
      delete masthead.dataset.searching;
      if (storyInput.value) {
        storyInput.value = "";
        view.q.value = "";
        reloadItems();
      }
    }
  }
  storyInput.addEventListener("keydown", (e) => {
    if (e.key === "Escape") toggleStorySearch(false);
  });

  // ---- filter sections (client side) ----
  function applySectionFilter() {
    const q = secFilter.value.trim().toLowerCase();
    document.querySelectorAll("#sections .sec-row[data-name]").forEach((row) => {
      row.hidden = q !== "" && !row.dataset.name.toLowerCase().includes(q);
    });
  }
  secFilter.addEventListener("input", applySectionFilter);
  secFilter.addEventListener("keydown", (e) => {
    if (e.key === "Escape") toggleSectionFilter(false);
  });

  function toggleSectionFilter(open) {
    const toggle = document.querySelector("[data-sections-search]");
    secFilter.hidden = !open;
    toggle.setAttribute("aria-expanded", String(open));
    if (open) secFilter.focus();
    else { secFilter.value = ""; applySectionFilter(); }
  }

  document.addEventListener("click", (e) => {
    const t = e.target;

    if (t === dialog) return dialog.close(); // click on the backdrop

    const filter = t.closest("[data-filter]");
    if (filter) return setFilter(filter.dataset.feed, filter.dataset.unread);

    if (t.closest("[data-search-toggle]")) return toggleStorySearch(storySearch.hidden);
    if (t.closest("[data-sections-search]")) return toggleSectionFilter(secFilter.hidden);

    if (t.closest("[data-open]")) {
      notice.replaceChildren();
      dialog.showModal();
      form.elements.url.focus();
      return;
    }
    if (t.closest("[data-close]")) return dialog.close();

    const story = t.closest("[data-story]");
    if (story) {
      document.querySelectorAll("[data-story][aria-current]").forEach((b) => b.removeAttribute("aria-current"));
      story.setAttribute("aria-current", "true");
      view.story.value = story.closest(".row").id.replace("item-", "");
      showPane("reader");
      return;
    }

    if (t.closest("[data-back]")) showPane("list");
  });

  // A successful add closes the dialog and clears the form.
  document.body.addEventListener("section-added", () => {
    dialog.close();
    form.reset();
    notice.replaceChildren();
  });

  // Removing a section resets the filter, so the list never points at a feed that's gone.
  document.body.addEventListener("htmx:afterRequest", (e) => {
    const { verb } = e.detail.requestConfig;
    if (verb === "delete" && e.detail.successful) {
      view.feed.value = "";
      view.unread.value = "";
    }
  });

  document.body.addEventListener("htmx:afterSwap", (e) => {
    const t = e.detail.target;
    if (t === reader) reader.scrollTop = 0; // a newly opened story starts at the top
    if (t === items && resetItemScroll) {
      items.scrollTop = 0;
      resetItemScroll = false;
    }
    if (t.id === "sections") applySectionFilter(); // keep the typed filter after a refresh
  });
})();
