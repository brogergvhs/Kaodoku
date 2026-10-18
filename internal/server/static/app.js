// dropdowns: close any open <details.dropdown> on outside click or action,
// except those marked data-keep-open (multi-action popovers like the tag filter).
document.addEventListener("click", function (e) {
  var inDrop = e.target.closest("details.dropdown");
  var keepOpen = e.target.closest("[data-keep-open]");
  var action = e.target.closest(".menu a, .menu button, .dropdown-content button");
  document.querySelectorAll("details.dropdown[open]").forEach(function (d) {
    if (d !== inDrop || (action && !keepOpen)) {
      d.removeAttribute("open");
    }
  });
});

// close the mobile drawer after navigating or acting from within it
document.addEventListener("click", function (e) {
  if (!e.target.closest(".drawer-side .menu a")) return;
  var toggle = document.getElementById("app-drawer");
  if (toggle && window.matchMedia("(max-width: 1023px)").matches)
    toggle.checked = false;
});

function showErrorToast(msg) {
  var toast = document.getElementById("toast");
  if (!toast) return;
  toast.innerHTML =
    '<div class="alert alert-error shadow-lg"><span></span></div>';
  toast.querySelector("span").textContent = msg;
}

document.body.addEventListener("htmx:sendError", function () {
  showErrorToast("Network error. Check the server connection.");
});
document.body.addEventListener("htmx:timeout", function () {
  showErrorToast("Request timed out. Try again.");
});
document.body.addEventListener("htmx:responseError", function (e) {
  if (e.detail.xhr && e.detail.xhr.status >= 500) {
    showErrorToast("Server error. Check the logs and try again.");
  }
});
document.body.addEventListener("htmx:beforeRequest", function (e) {
  if (!e.target.matches("[data-backup-upload]")) return;
  var bar = document.getElementById("backup-upload-progress");
  if (!bar) return;
  bar.hidden = false;
  bar.value = 0;
});
document.body.addEventListener("htmx:xhr:progress", function (e) {
  if (!e.target.matches("[data-backup-upload]")) return;
  var bar = document.getElementById("backup-upload-progress");
  if (!bar || !e.detail.lengthComputable || !e.detail.total) return;
  bar.hidden = false;
  bar.value = Math.round((e.detail.loaded * 100) / e.detail.total);
});

// confirm modal: replaces native hx-confirm (browsers can suppress the
// native dialog); renders an in-app daisyUI modal instead
document.addEventListener("htmx:confirm", function (e) {
  var question = e.detail.question;
  if (!question) return;
  e.preventDefault();
  var dlg = document.createElement("dialog");
  dlg.className = "modal modal-open";
  dlg.innerHTML =
    '<div class="modal-box"><p class="py-2"></p>' +
    '<div class="modal-action"><button class="btn btn-ghost" data-cancel>Cancel</button>' +
    '<button class="btn btn-error" data-ok>Confirm</button></div></div>' +
    '<div class="modal-backdrop" data-cancel></div>';
  dlg.querySelector("p").textContent = question;
  function close() {
    dlg.remove();
    document.removeEventListener("keydown", onKey);
  }
  function onKey(ev) {
    if (ev.key === "Escape") close();
  }
  dlg.addEventListener("click", function (ev) {
    if (ev.target.closest("[data-cancel]")) {
      close();
      return;
    }
    if (ev.target.closest("[data-ok]")) {
      close();
      e.detail.issueRequest(true);
    }
  });
  document.addEventListener("keydown", onKey);
  document.body.appendChild(dlg);
  dlg.querySelector("[data-ok]").focus();
});

// Collapsible table rows: a .row-toggle reveals the detail row it points at.
// Clicks on interactive elements inside the row are ignored. Open rows are
// remembered so frequent table refreshes (polling) don't collapse them.
var openDetails = new Set();
document.addEventListener("click", function (e) {
  if (
    e.target.closest(
      "button, a, input, select, textarea, form, label, details, summary",
    )
  )
    return;
  var toggle = e.target.closest(".row-toggle");
  if (!toggle) return;
  var target = document.getElementById(toggle.dataset.target);
  if (!target) return;
  target.hidden = !target.hidden;
  toggle.classList.toggle("open", !target.hidden);
  if (target.hidden) {
    openDetails.delete(target.id);
  } else {
    openDetails.add(target.id);
  }
});
document.body.addEventListener("htmx:afterSwap", function () {
  openDetails.forEach(function (id) {
    var el = document.getElementById(id);
    if (!el) return;
    el.hidden = false;
    var toggle = document.querySelector('[data-target="' + id + '"]');
    if (toggle) toggle.classList.add("open");
  });
});

// Kotatsu picker: selections live in a Set on the modal element so they
// survive filter/search swaps of #kotatsu-browse; hidden inputs in
// #kotatsu-selected carry the full set into the import request.
function kotatsuSel() {
  var modal = document.getElementById("kotatsu-add-modal");
  if (!modal) return null;
  return (modal._sel = modal._sel || new Set());
}
function kotatsuSync() {
  var sel = kotatsuSel();
  var browse = document.getElementById("kotatsu-browse");
  if (!sel || !browse) return;
  browse.querySelectorAll("input[name='id']").forEach(function (c) {
    if (c.disabled) {
      sel.delete(c.value);
      return;
    }
    c.checked = sel.has(c.value);
  });
  var holder = document.getElementById("kotatsu-selected");
  if (holder) {
    holder.innerHTML = "";
    sel.forEach(function (id) {
      var inp = document.createElement("input");
      inp.type = "hidden";
      inp.name = "id";
      inp.value = id;
      holder.appendChild(inp);
    });
  }
  var n = sel.size;
  var btn = document.getElementById("kotatsu-import-btn");
  if (btn) {
    btn.disabled = n === 0;
    var label = btn.querySelector("#kotatsu-import-label") || btn;
    label.textContent = n ? "Import " + n : "Import";
  }
  var clear = document.getElementById("kotatsu-clear");
  if (clear) clear.hidden = n === 0;
}
document.addEventListener("change", function (e) {
  if (!e.target.matches("#kotatsu-browse input[name='id']")) return;
  var sel = kotatsuSel();
  if (!sel) return;
  if (e.target.checked) sel.add(e.target.value);
  else sel.delete(e.target.value);
  kotatsuSync();
});
document.addEventListener("click", function (e) {
  if (!e.target.closest("#kotatsu-clear")) return;
  var sel = kotatsuSel();
  if (sel) sel.clear();
  kotatsuSync();
});
document.body.addEventListener("htmx:afterSwap", kotatsuSync);

// Tag picker: the filter input hides non-matching checkbox rows or chips.
document.addEventListener("input", function (e) {
  var filter = e.target.closest("[data-tag-filter]");
  if (!filter) return;
  var scope = filter.closest(".dropdown-content") || filter.parentElement;
  if (!scope) return;
  var q = filter.value.trim().toLowerCase();
  scope.querySelectorAll("label, [data-tag-chip]").forEach(function (row) {
    var el = row.matches("[data-tag-chip]") ? row.parentElement : row;
    el.hidden = q !== "" && row.textContent.toLowerCase().indexOf(q) === -1;
  });
  var divider = scope.querySelector("[data-tag-divider]");
  if (divider) divider.hidden = q !== "";
  // tag filter: a matching category header reveals its whole group; then hide
  // groups with no visible chips and show the empty state when nothing matches.
  if (filter.closest("[data-tagfilter]")) {
    var anyVisible = false;
    scope.querySelectorAll("[data-tag-list]").forEach(function (group) {
      var header = group.querySelector("span");
      if (q !== "" && header && header.textContent.toLowerCase().indexOf(q) !== -1) {
        group.querySelectorAll("li").forEach(function (li) {
          li.hidden = false;
        });
      }
      var hasVisible = !!group.querySelector("li:not([hidden])");
      group.hidden = !hasVisible;
      if (hasVisible) anyVisible = true;
    });
    var empty = scope.querySelector("[data-tagfilter-empty]");
    if (empty) empty.hidden = anyVisible;
  }
});

// Tag filter: tri-state chips cycle neutral -> include -> exclude -> neutral.
var TAG_CHIP_BASE = "cursor-pointer rounded-md border px-2 py-0.5 text-xs transition-colors";
var TAG_CHIP_CLASS = {
  include: "border-solid border-success text-success bg-success/10",
  exclude: "border-solid border-error text-error bg-error/10",
  "": "border-dashed border-base-300 text-base-content/70",
};
function tagFilterApply(root) {
  var hidden = root.querySelector("[data-tagfilter-hidden]");
  hidden.innerHTML = "";
  var inc = 0;
  var exc = 0;
  root.querySelectorAll("[data-tag-chip]").forEach(function (chip) {
    var state = chip.dataset.state;
    chip.className = TAG_CHIP_BASE + " " + TAG_CHIP_CLASS[state];
    if (state !== "include" && state !== "exclude") return;
    var input = document.createElement("input");
    input.type = "hidden";
    input.name = state === "include" ? "include_tags" : "exclude_tags";
    input.value = chip.dataset.tagName;
    hidden.appendChild(input);
    if (state === "include") inc++;
    else exc++;
  });
  var summary = root.querySelector("[data-tagfilter-summary]");
  if (summary) {
    var parts = [];
    if (inc) parts.push(inc + " included");
    if (exc) parts.push(exc + " excluded");
    summary.textContent = parts.length ? parts.join(" · ") : summary.dataset.emptyLabel;
  }
  var trigger = root.querySelector("[data-tagfilter-trigger]");
  if (trigger) {
    trigger.value = String(Number(trigger.value || "0") + 1);
    trigger.dispatchEvent(new Event("change", { bubbles: true }));
  }
}
document.addEventListener("click", function (e) {
  var chip = e.target.closest("[data-tag-chip]");
  if (chip) {
    e.preventDefault();
    chip.dataset.state =
      chip.dataset.state === "include"
        ? "exclude"
        : chip.dataset.state === "exclude"
          ? ""
          : "include";
    tagFilterApply(chip.closest("[data-tagfilter]"));
    return;
  }
  var reset = e.target.closest("[data-tagfilter-reset]");
  if (reset) {
    e.preventDefault();
    var root = reset.closest("[data-tagfilter]");
    root.querySelectorAll("[data-tag-chip]").forEach(function (c) {
      c.dataset.state = "";
    });
    tagFilterApply(root);
  }
});

function filterChips(form) {
  var chips = [];
  form.querySelectorAll("select[data-default]").forEach(function (sel) {
    if (sel.value === sel.dataset.default) return;
    var legend = sel.closest(".fieldset");
    legend = legend ? legend.querySelector(".fieldset-legend").textContent : sel.name;
    chips.push({ k: "sel", name: sel.name, text: legend + ": " + sel.selectedOptions[0].textContent, cls: "badge-ghost" });
  });
  form.querySelectorAll("input[data-filter-toggle]:checked").forEach(function (t) {
    chips.push({ k: "tog", name: t.name, text: t.closest("label").textContent.trim(), cls: "badge-ghost" });
  });
  form.querySelectorAll("[data-tag-list] input[name='source']:checked").forEach(function (c) {
    chips.push({ k: "src", val: c.value, text: "Source: " + c.closest("label").textContent.trim(), cls: "badge-ghost" });
  });
  form.querySelectorAll("[data-tagfilter-hidden] input[name='include_tags']").forEach(function (i) {
    chips.push({ k: "inc", val: i.value, text: i.value, cls: "badge-soft badge-success" });
  });
  form.querySelectorAll("[data-tagfilter-hidden] input[name='exclude_tags']").forEach(function (i) {
    chips.push({ k: "exc", val: i.value, text: i.value, cls: "badge-soft badge-error" });
  });
  return chips;
}
function updateFilterCounts() {
  document.querySelectorAll("[data-filter-count]").forEach(function (span) {
    var form = span.closest("form");
    if (!form) return;
    var chips = filterChips(form);
    span.textContent = chips.length ? "Filters · " + chips.length : "Filters";
    var strip = form.nextElementSibling;
    if (!strip || !strip.hasAttribute("data-active-filters")) return;
    strip.innerHTML = "";
    strip.classList.toggle("hidden", chips.length === 0);
    strip.classList.toggle("flex", chips.length > 0);
    chips.forEach(function (c) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "badge badge-sm cursor-pointer gap-1 " + c.cls;
      b.dataset.k = c.k;
      if (c.name) b.dataset.name = c.name;
      if (c.val !== undefined) b.dataset.val = c.val;
      b.append(c.text);
      var x = document.createElement("span");
      x.className = "opacity-60";
      x.textContent = "\u2715";
      b.appendChild(x);
      strip.appendChild(b);
    });
    var clear = document.createElement("button");
    clear.type = "button";
    clear.className = "btn btn-ghost btn-xs";
    clear.dataset.k = "clear";
    clear.textContent = "Clear all";
    strip.appendChild(clear);
  });
}
document.addEventListener("change", function (e) {
  if (e.target.closest("form")) updateFilterCounts();
});
document.addEventListener("click", function (e) {
  var strip = e.target.closest("[data-active-filters]");
  if (!strip) return;
  var chip = e.target.closest("[data-k]");
  if (!chip) return;
  var form = strip.previousElementSibling;
  var fire = function (el) {
    el.dispatchEvent(new Event("change", { bubbles: true }));
  };
  var tagOff = function (name) {
    var tc = form.querySelector("[data-tag-chip][data-tag-name='" + CSS.escape(name) + "']");
    if (tc) {
      tc.dataset.state = "";
      tagFilterApply(tc.closest("[data-tagfilter]"));
    }
  };
  switch (chip.dataset.k) {
    case "sel": {
      var sel = form.querySelector("select[name='" + chip.dataset.name + "']");
      sel.value = sel.dataset.default;
      fire(sel);
      break;
    }
    case "tog": {
      var t = form.querySelector("input[data-filter-toggle][name='" + chip.dataset.name + "']");
      t.checked = false;
      fire(t);
      break;
    }
    case "src": {
      var c = form.querySelector("input[name='source'][value='" + CSS.escape(chip.dataset.val) + "']");
      c.checked = false;
      fire(c);
      break;
    }
    case "inc":
    case "exc":
      tagOff(chip.dataset.val);
      break;
    case "clear": {
      form.querySelectorAll("select[data-default]").forEach(function (sel) {
        sel.value = sel.dataset.default;
      });
      form.querySelectorAll("input[data-filter-toggle]:checked, [data-tag-list] input[name='source']:checked").forEach(function (c) {
        c.checked = false;
      });
      var summary = form.querySelector("[data-tag-count]");
      if (summary) summary.textContent = summary.dataset.emptyLabel || "None selected";
      var tf = form.querySelector("[data-tagfilter]");
      if (tf && tf.querySelector("[data-tag-chip][data-state='include'], [data-tag-chip][data-state='exclude']")) {
        tf.querySelectorAll("[data-tag-chip]").forEach(function (c) {
          c.dataset.state = "";
        });
        tagFilterApply(tf);
      } else {
        fire(form.querySelector("select[data-default]"));
      }
      break;
    }
  }
  updateFilterCounts();
});
updateFilterCounts();

// Tag picker: keep the "N selected" summary in sync with the checkboxes.
document.addEventListener("change", function (e) {
  if (!e.target.matches("[data-tag-list] input[type='checkbox']")) return;
  var details = e.target.closest("details.dropdown");
  if (!details) return;
  var summary = details.querySelector("[data-tag-count]");
  if (!summary) return;
  var count = details.querySelectorAll("[data-tag-list] input:checked").length;
  summary.textContent = count
    ? count + " selected"
    : summary.dataset.emptyLabel || "None selected";
});

function updateLibraryBulk() {
  var form = document.getElementById("library-bulk");
  if (!form) return;
  var seen = {};
  var count = 0;
  document.querySelectorAll("[data-bulk-title]:checked").forEach(function (c) {
    if (seen[c.value]) return;
    seen[c.value] = true;
    count++;
  });
  form.hidden = count === 0;
  var button = form.querySelector("[data-bulk-apply]");
  if (button)
    button.textContent =
      "Apply to " + count + " selected title" + (count === 1 ? "" : "s");
}
document.addEventListener("change", function (e) {
  if (e.target.matches("[data-bulk-title]")) updateLibraryBulk();
});
document.addEventListener("click", function (e) {
  if (!e.target.matches("[data-bulk-cancel]")) return;
  document.querySelectorAll("[data-bulk-title]:checked").forEach(function (c) {
    c.checked = false;
  });
  updateLibraryBulk();
});
document.body.addEventListener("htmx:afterSwap", updateLibraryBulk);
updateLibraryBulk();

// Library filters: mirror table refreshes into the URL so reloads and
// back-navigation keep them; plain sidebar links reset them.
document.body.addEventListener("htmx:afterRequest", function (e) {
  if (!e.detail.successful || !e.detail.xhr || !e.detail.xhr.responseURL)
    return;
  if (e.detail.elt && e.detail.elt.hasAttribute("data-poll")) return;
  var url;
  try {
    url = new URL(e.detail.xhr.responseURL);
  } catch (err) {
    return;
  }
  if (url.pathname !== "/ui/library/table") return;
  var current = new URLSearchParams(location.search);
  ["new-screen", "edit"].forEach(function (key) {
    if (current.has(key)) url.searchParams.set(key, current.get(key));
  });
  var qs = url.searchParams.toString();
  history.replaceState(null, "", "/library" + (qs ? "?" + qs : ""));
});

// Search filters: mirror the form state into the /search URL.
document.body.addEventListener("htmx:afterRequest", function (e) {
  if (location.pathname !== "/search") return;
  if (!e.detail.successful || !e.detail.xhr || !e.detail.xhr.responseURL)
    return;
  var path;
  try {
    path = new URL(e.detail.xhr.responseURL).pathname;
  } catch (err) {
    return;
  }
  if (path !== "/ui/search" && path !== "/ui/search/trending") return;
  var form = document.getElementById("search-form");
  if (!form) return;
  var qs = new URLSearchParams(new FormData(form)).toString();
  history.replaceState(null, "", qs ? "/search?" + qs : "/search");
});

// Sidebar screen reordering: drag rows, persist the new order on drop.
(function () {
  var list = document.querySelector("[data-screen-list]");
  if (!list) return;
  var dragging = null;
  list.addEventListener("dragstart", function (e) {
    dragging = e.target.closest("[data-screen-id]");
    if (dragging) dragging.classList.add("dragging");
  });
  list.addEventListener("dragend", function () {
    if (dragging) dragging.classList.remove("dragging");
    dragging = null;
  });
  list.addEventListener("dragover", function (e) {
    if (!dragging) return;
    e.preventDefault();
    var over = e.target.closest("[data-screen-id]");
    if (!over || over === dragging) return;
    var rect = over.getBoundingClientRect();
    var after = e.clientY > rect.top + rect.height / 2;
    list.insertBefore(dragging, after ? over.nextSibling : over);
  });
  list.addEventListener("drop", function (e) {
    e.preventDefault();
    var params = new URLSearchParams();
    list.querySelectorAll("[data-screen-id]").forEach(function (row) {
      params.append("order", row.dataset.screenId);
    });
    fetch("/ui/screens/reorder", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: params.toString(),
    });
  });
})();

(function () {
  var shell = document.querySelector("[data-reader]");
  if (!shell) return;
  var manifestTag = document.getElementById("reader-manifest");
  var strip = document.getElementById("reader-strip");
  var track = document.getElementById("reader-track");
  var loadingEl = document.getElementById("reader-loading");
  var position = document.getElementById("reader-position");
  var prev = document.querySelector("[data-reader-prev]");
  var next = document.querySelector("[data-reader-next]");

  function pref(k, d) {
    try {
      return localStorage.getItem(k) || d;
    } catch (e) {
      return d;
    }
  }
  function setPref(k, v) {
    try {
      localStorage.setItem(k, v);
    } catch (e) {}
  }
  var mode = pref("reader-mode", "strip");
  var dir = pref("reader-dir", "ltr");
  var fit = pref("reader-fit", "contain");
  var zoom = parseFloat(pref("reader-zoom", "1")) || 1;
  var smooth = window.matchMedia("(prefers-reduced-motion: reduce)").matches
    ? "auto"
    : "smooth";
  var curEl = null;

  function applyView() {
    strip.dataset.mode = mode;
    strip.dataset.dir = dir;
    strip.dataset.fit = fit;
    strip.style.setProperty("--reader-zoom", zoom);
    document.body.classList.toggle("reader-paged", mode === "paged");
    shell.querySelectorAll("[data-set]").forEach(function (b) {
      var kv = b.dataset.set.split("=");
      var on =
        (kv[0] === "mode" && kv[1] === mode) ||
        (kv[0] === "dir" && kv[1] === dir) ||
        (kv[0] === "fit" && kv[1] === fit);
      b.classList.toggle("btn-outline", on);
      b.classList.toggle("btn-primary", on);
    });
    var zl = shell.querySelector("[data-zoom-label]");
    if (zl) zl.textContent = Math.round(zoom * 100) + "%";
  }

  function toggleControls() {
    if (window.matchMedia("(max-width: 1024px)").matches)
      document.body.classList.toggle("controls-on");
  }

  // controls & keyboard work even on the empty page
  document.addEventListener("click", function (e) {
    if (e.target.closest(".reader-controls")) return;
    if (mode === "paged" && pages && pages.length) {
      if (swiped) {
        swiped = false;
        return;
      }
      var ctl = shell.querySelector(".reader-controls");
      if (ctl && e.clientY < ctl.offsetHeight + 12) {
        toggleControls();
        return;
      }
      var x = e.clientX / window.innerWidth;
      if (x < 0.33) turn(dir === "rtl" ? 1 : -1);
      else if (x > 0.67) turn(dir === "rtl" ? -1 : 1);
      else toggleControls();
      return;
    }
    toggleControls();
  });

  var downX = null,
    swiped = false;
  strip.addEventListener("pointerdown", function (e) {
    swiped = false;
    if (mode !== "paged" || zoom > 1) {
      downX = null;
      return;
    }
    downX = e.clientX;
  });
  strip.addEventListener("pointerup", function (e) {
    if (mode !== "paged" || downX === null) return;
    var dx = e.clientX - downX;
    downX = null;
    if (Math.abs(dx) < 40) return;
    swiped = true;
    turn(dx < 0 ? (dir === "rtl" ? -1 : 1) : dir === "rtl" ? 1 : -1);
  });
  document.addEventListener("keydown", function (e) {
    if (e.target.closest("input, textarea, select")) return;
    if (mode === "paged" && pages && pages.length) {
      if (e.key === "ArrowRight") turn(dir === "rtl" ? -1 : 1);
      else if (e.key === "ArrowLeft") turn(dir === "rtl" ? 1 : -1);
      else if (e.key === " ") {
        e.preventDefault();
        turn(1);
      } else if (e.key === "[" && prev) location.href = prev.href;
      else if (e.key === "]" && next) goNext();
      return;
    }
    if ((e.key === "ArrowLeft" || e.key === "[") && prev) {
      location.href = prev.href;
    } else if ((e.key === "ArrowRight" || e.key === "]") && next) {
      goNext();
    } else if (e.key === "j") {
      window.scrollBy({ top: window.innerHeight * 0.9, behavior: smooth });
    } else if (e.key === "k") {
      window.scrollBy({ top: -window.innerHeight * 0.9, behavior: smooth });
    }
  });

  shell.addEventListener("click", function (e) {
    var s = e.target.closest("[data-set]");
    var z = e.target.closest("[data-zoom]");
    if (!s && !z) return;
    e.stopPropagation();
    if (s) {
      var kv = s.dataset.set.split("=");
      if (kv[0] === "mode") {
        mode = kv[1];
        setPref("reader-mode", mode);
      } else if (kv[0] === "dir") {
        dir = kv[1];
        setPref("reader-dir", dir);
      } else if (kv[0] === "fit") {
        fit = kv[1];
        setPref("reader-fit", fit);
      }
    } else {
      zoom = Math.min(
        3,
        Math.max(
          1,
          Math.round((zoom + parseInt(z.dataset.zoom, 10) * 0.25) * 100) / 100,
        ),
      );
      setPref("reader-zoom", String(zoom));
    }
    applyView();
    refreshView();
  });

  function buildFailed(msg) {
    if (loadingEl) {
      loadingEl.textContent = msg;
      loadingEl.classList.add("reader-page-failed");
    }
  }
  if (!manifestTag || !strip) {
    buildFailed("Reader failed to initialise.");
    return;
  }
  var manifest;
  try {
    manifest = JSON.parse(manifestTag.textContent);
  } catch (err) {
    buildFailed("Reader failed to parse the chapter manifest.");
    return;
  }

  // Flatten the manifest into an ordered build queue. Elements are appended
  // strictly in order, and each image is appended only after it has fully
  // loaded (dimensions known), so the layout never collapses and a later
  // chapter can never appear before every earlier page is in place.
  var queue = [];
  var seenChapters = {};
  var lastChapterID = 0;
  function enqueueChapters(chapters) {
    var added = 0;
    (chapters || []).forEach(function (chapter) {
      if (seenChapters[chapter.id]) return;
      seenChapters[chapter.id] = true;
      lastChapterID = chapter.id;
      added++;
      queue.push({ kind: "sep", label: chapter.label });
      (chapter.pages || []).forEach(function (page) {
        queue.push({
          kind: "page",
          chapter: String(chapter.id),
          label: chapter.label,
          page: page.page,
          total: chapter.page_count,
          url: page.url,
          read: !!page.read,
        });
      });
    });
    return added;
  }
  enqueueChapters(manifest.chapters);

  var resumeChapter = shell.dataset.resumeChapter;
  var resumePage = parseInt(shell.dataset.resumePage || "0", 10);
  var pages = []; // appended <img> elements, in order
  var read = {};
  var pending = {};
  var writeQueue = Promise.resolve();

  function postJSON(url, body) {
    return fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body || {}),
      keepalive: true, // final page marks must survive chapter navigation
    }).then(function (resp) {
      if (!resp.ok) throw new Error("reader write failed");
      return resp.json();
    });
  }

  function queueWrite(fn) {
    writeQueue = writeQueue.then(fn, fn);
    return writeQueue;
  }

  function mark(img) {
    var chapter = img.dataset.chapter;
    var page = parseInt(img.dataset.page || "0", 10);
    var total = parseInt(img.dataset.total || "0", 10);
    var key = chapter + ":" + page;
    if (!chapter || !page || read[key] || pending[key]) return;
    pending[key] = true;
    queueWrite(function () {
      return postJSON(
        (manifest.mark_base || "/api/reader/chapters/") + chapter + "/pages",
        {
          page: page,
          total_pages: total,
        },
      )
        .then(function () {
          read[key] = true;
          img.classList.add("is-read");
          delete pending[key];
        })
        .catch(function () {
          delete pending[key];
        });
    });
  }

  function updatePosition() {
    if (!position || pages.length === 0) return;
    var marker = window.innerHeight * 0.45;
    var best = null;
    var bestDistance = Infinity;
    for (var i = 0; i < pages.length; i++) {
      var rect = pages[i].getBoundingClientRect();
      if (rect.height <= 0) continue;
      if (rect.top <= marker && rect.bottom >= marker) {
        best = pages[i];
        break;
      }
      var distance = Math.min(
        Math.abs(rect.top - marker),
        Math.abs(rect.bottom - marker),
      );
      if (distance < bestDistance) {
        best = pages[i];
        bestDistance = distance;
      }
    }
    if (best) {
      curEl = best;
      position.textContent = best.dataset.page + "/" + best.dataset.total;
    }
  }

  var ticking = false;
  function checkVisible() {
    ticking = false;
    if (mode !== "strip") return;
    updatePosition();
    var threshold = window.innerHeight * 0.85;
    var doc = document.documentElement;
    var atBottom =
      built && window.scrollY + window.innerHeight >= doc.scrollHeight - 4;
    pages.forEach(function (img) {
      var rect = img.getBoundingClientRect();
      // Anything whose bottom rose above the 85% line counts as read — with
      // no lower bound, so short pages that jump across the whole band (or
      // off-screen entirely) between two scroll frames aren't skipped.
      if (
        rect.bottom <= threshold ||
        (atBottom && rect.top < window.innerHeight)
      ) {
        mark(img);
      }
    });
  }
  function requestCheck() {
    if (ticking) return;
    ticking = true;
    requestAnimationFrame(checkVisible);
  }
  window.addEventListener("scroll", requestCheck, { passive: true });
  window.addEventListener("resize", requestCheck);

  // Moving on to the next chapter finishes the current screen: short final
  // pages sit fully visible without ever crossing the 85% line, so sweep
  // everything on or above the viewport, flush the write queue, then follow
  // the link (keepalive covers writes the browser would otherwise cancel).
  function goNext() {
    if (!next) return;
    if (!pages) {
      // the empty page initialises no strip state
      location.href = next.href;
      return;
    }
    if (mode === "paged") {
      if (curEl) mark(curEl);
    } else {
      pages.forEach(function (img) {
        if (img.getBoundingClientRect().top < window.innerHeight) mark(img);
      });
    }
    queueWrite(function () {
      location.href = next.href;
    });
  }
  if (next) {
    next.addEventListener("click", function (e) {
      e.preventDefault();
      goNext();
    });
  }

  // sequential builder: preload a few ahead, append strictly in order.
  // Appending is gated on scroll proximity so a long title isn't downloaded
  // in the background, and the strip keeps extending with the next chapter
  // as the reader approaches the end — no need to reopen the reader.
  var LOOKAHEAD = 3;
  var GATE_PX = window.innerHeight * 2.5;
  var loaders = {}; // queue index -> Promise settling when its image is loaded
  var appended = 0;
  var built = false;
  var waiting = false; // paused until the reader scrolls closer to the end
  var fetching = false; // a manifest extension request is in flight
  var noMore = false; // the server has no further chapters
  var resumed = !resumeChapter || !resumePage;
  var cur = 0;
  var pendingTurn = false;

  function nearEnd() {
    if (mode === "paged") return pages.length - 1 - cur <= LOOKAHEAD + 1;
    var doc = document.documentElement;
    return doc.scrollHeight - (window.scrollY + window.innerHeight) < GATE_PX;
  }

  function extend() {
    if (fetching) return;
    if (noMore || !lastChapterID) {
      finish();
      return;
    }
    fetching = true;
    fetch(
      manifest.extend_base
        ? manifest.extend_base + lastChapterID
        : "/api/reader/titles/" +
            manifest.title_id +
            "/manifest?chapter=" +
            lastChapterID,
    )
      .then(function (resp) {
        if (!resp.ok) throw new Error("manifest fetch failed");
        return resp.json();
      })
      .then(function (m) {
        fetching = false;
        if (enqueueChapters(m.chapters) > 0) {
          appendNext();
        } else {
          noMore = true;
          finish();
        }
      })
      .catch(function () {
        fetching = false;
        noMore = true;
        finish();
      });
  }

  function finish() {
    built = true;
    if (pendingTurn) {
      pendingTurn = false;
      goNext();
    }
    if (loadingEl) loadingEl.remove();
    requestCheck();
  }

  function loaderFor(i) {
    var item = queue[i];
    if (item.kind !== "page") return Promise.resolve(null);
    if (!loaders[i]) {
      loaders[i] = new Promise(function (resolve) {
        var img = new Image();
        img.onload = function () {
          if (img.decode) {
            img.decode().then(
              function () {
                resolve(img);
              },
              function () {
                resolve(img);
              },
            );
          } else {
            resolve(img);
          }
        };
        img.onerror = function () {
          resolve(null);
        };
        img.src = item.url;
      });
    }
    return loaders[i];
  }

  function appendNext() {
    if (appended >= queue.length) {
      extend();
      return;
    }
    // Past the resume point, only build while the reader is near the end.
    if (resumed && !nearEnd()) {
      waiting = true;
      return;
    }
    // Warm the pipeline (network only; nothing enters the DOM out of order).
    for (
      var i = appended;
      i < Math.min(appended + 1 + LOOKAHEAD, queue.length);
      i++
    ) {
      loaderFor(i);
    }
    var idx = appended;
    var item = queue[idx];
    if (item.kind === "sep") {
      var sep = document.createElement("div");
      sep.className = "reader-separator";
      sep.textContent = "Chapter " + item.label;
      track.appendChild(sep);
      appended++;
      appendNext();
      return;
    }
    loaderFor(idx).then(function (img) {
      var el = document.createElement("div");
      el.className = "reader-page" + (item.read ? " is-read" : "");
      if (img) {
        img.className = "reader-img";
        img.setAttribute("alt", "Chapter " + item.label + " page " + item.page);
        el.appendChild(img);
      } else {
        el.classList.add("reader-page-failed");
        el.textContent = "Page " + item.page + " failed to load";
      }
      el.id = "chapter-" + item.chapter + "-page-" + item.page;
      el.dataset.chapter = item.chapter;
      el.dataset.chapterLabel = item.label;
      el.dataset.page = String(item.page);
      el.dataset.total = String(item.total);
      if (item.read) read[item.chapter + ":" + item.page] = true;
      track.appendChild(el);
      pages.push(el); // failed pages count too
      delete loaders[idx];
      appended++;
      if (
        !resumed &&
        String(item.chapter) === resumeChapter &&
        item.page === resumePage
      ) {
        resumed = true;
        if (mode === "paged") focus(pages.length - 1);
        else el.scrollIntoView({ block: "start" });
      } else if (mode === "paged" && resumed && cur === pages.length - 1) {
        focus(cur);
      }
      if (pendingTurn && cur >= pages.length - 2) {
        pendingTurn = false;
        turn(1);
      }
      requestCheck();
      appendNext();
    });
  }

  window.addEventListener(
    "scroll",
    function () {
      if (waiting && nearEnd()) {
        waiting = false;
        appendNext();
      }
    },
    { passive: true },
  );

  function focus(i) {
    if (i < 0 || i >= pages.length) return;
    pendingTurn = false;
    cur = i;
    curEl = pages[i];
    track.style.transform =
      "translateX(" + (dir === "rtl" ? i * 100 : i * -100) + "%)";
    if (curEl.dataset.page) {
      mark(curEl);
      if (position)
        position.textContent = curEl.dataset.page + "/" + curEl.dataset.total;
    }
    if (waiting && nearEnd()) {
      waiting = false;
      appendNext();
    }
  }

  function turn(delta) {
    var t = cur + delta;
    if (t < 0) {
      if (prev) location.href = prev.href;
      return;
    }
    if (t >= pages.length) {
      if (!noMore && lastChapterID) {
        pendingTurn = true;
        waiting = false;
        appendNext();
        return;
      }
      goNext();
      return;
    }
    focus(t);
  }

  function refreshView() {
    if (mode === "paged") {
      var i = curEl ? pages.indexOf(curEl) : cur;
      focus(i < 0 ? 0 : Math.min(i, pages.length - 1));
    } else {
      track.style.transform = "";
      if (curEl) curEl.scrollIntoView({ block: "start" });
      requestCheck();
    }
  }

  applyView();
  appendNext();
})();
