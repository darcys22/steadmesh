// Steadmesh Console: periodic refresh of the seat grid and the live activity
// stream. The pages work without it; it only keeps them current.
(function () {
  "use strict";

  // Seat grid: re-fetch the server-rendered fragment.
  var grid = document.querySelector("[data-refresh]");
  if (grid) {
    var every = parseInt(grid.getAttribute("data-interval"), 10) || 5000;
    setInterval(function () {
      if (document.hidden) return;
      fetch(grid.getAttribute("data-refresh"), { credentials: "same-origin" })
        .then(function (r) { return r.ok ? r.text() : Promise.reject(r.status); })
        .then(function (html) { grid.innerHTML = html; })
        .catch(function () {});
    }, every);
  }

  // Activity: Server-Sent Events prepend new entries and light up the map.
  var feed = document.getElementById("feed");
  if (!feed || !window.EventSource) return;
  var status = document.getElementById("live-status");
  var empty = document.getElementById("feed-empty");
  var MAX_ITEMS = 500;

  function pulse(el) {
    if (!el) return;
    el.classList.add("pulse");
    clearTimeout(el._pulse);
    el._pulse = setTimeout(function () { el.classList.remove("pulse"); }, 1600);
  }
  function edge(a, b) {
    if (!a || !b) return null;
    var pair = a < b ? [a, b] : [b, a];
    return document.getElementById("edge-" + pair[0] + "--" + pair[1]);
  }

  var es = new EventSource(feed.getAttribute("data-stream"));
  es.onopen = function () { status.textContent = "live"; status.classList.add("on"); };
  es.onerror = function () { status.textContent = "reconnecting…"; status.classList.remove("on"); };
  es.addEventListener("activity", function (e) {
    var ev;
    try { ev = JSON.parse(e.data); } catch (_) { return; }
    var tpl = document.createElement("template");
    tpl.innerHTML = ev.html.trim();
    var li = tpl.content.firstElementChild;
    if (!li) return;
    li.classList.add("fresh");
    feed.insertBefore(li, feed.firstChild);
    while (feed.children.length > MAX_ITEMS) feed.removeChild(feed.lastChild);
    if (empty) { empty.remove(); empty = null; }
    if (ev.kind === "message") pulse(edge(ev.seat, ev.peer));
    pulse(document.getElementById("node-" + ev.seat));
    if (ev.kind === "message") pulse(document.getElementById("node-" + ev.peer));
  });
})();
