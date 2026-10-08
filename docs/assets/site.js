// Progressive enhancement only: every page works without this script.
// Collapses the docs sidebar behind a menu button on narrow screens, and adds
// copy buttons to code blocks. The current page is marked in the HTML
// (aria-current), so nothing here depends on the URL.
(function () {
  var root = document.documentElement;
  root.classList.remove("no-js");
  root.classList.add("js");

  var toggle = document.querySelector(".menu-toggle");
  var sidebar = document.querySelector(".sidebar");
  if (toggle && sidebar) {
    var setOpen = function (open, returnFocus) {
      sidebar.classList.toggle("open", open);
      toggle.setAttribute("aria-expanded", open ? "true" : "false");
      if (open) {
        var current = sidebar.querySelector('[aria-current="page"]') || sidebar.querySelector("a");
        if (current) current.focus();
      } else if (returnFocus) {
        toggle.focus();
      }
    };
    toggle.addEventListener("click", function () {
      setOpen(toggle.getAttribute("aria-expanded") !== "true", false);
    });
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && toggle.getAttribute("aria-expanded") === "true") setOpen(false, true);
    });
  }

  // One polite live region announces copy results without moving focus.
  var status = document.createElement("div");
  status.className = "copy-status";
  status.setAttribute("role", "status");
  status.setAttribute("aria-live", "polite");
  document.body.appendChild(status);
  var announce = function (msg) {
    status.textContent = "";
    setTimeout(function () { status.textContent = msg; }, 50);
  };

  document.querySelectorAll("pre:not([data-no-copy])").forEach(function (pre) {
    var btn = document.createElement("button");
    btn.className = "copy";
    btn.type = "button";
    var label = pre.hasAttribute("data-excerpt") ? "Copy excerpt" : "Copy";
    btn.textContent = label;
    var done = function (msg) {
      btn.textContent = msg;
      announce(msg === "Copied" ? "Copied to clipboard" : "Copy failed; select the text and copy it manually");
      setTimeout(function () { btn.textContent = label; }, 1800);
    };
    btn.addEventListener("click", function () {
      var code = pre.querySelector("code");
      var text = (code || pre).innerText;
      if (!navigator.clipboard || !navigator.clipboard.writeText) return done("Copy failed");
      navigator.clipboard.writeText(text).then(function () { done("Copied"); }, function () { done("Copy failed"); });
    });
    pre.appendChild(btn);
  });
})();
