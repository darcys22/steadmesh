// Highlights the current page in the sidebar, adds copy buttons to code
// blocks and toggles the sidebar on narrow screens. The site works without it.
(function () {
  var page = location.pathname.split("/").pop() || "index.html";
  document.querySelectorAll(".sidebar a[href]").forEach(function (a) {
    if (a.getAttribute("href") === page) a.classList.add("active");
  });

  document.querySelectorAll("pre").forEach(function (pre) {
    var btn = document.createElement("button");
    btn.className = "copy";
    btn.type = "button";
    btn.textContent = "Copy";
    btn.addEventListener("click", function () {
      var text = pre.querySelector("code") ? pre.querySelector("code").innerText : pre.innerText;
      try {
        navigator.clipboard.writeText(text).then(function () {
          btn.textContent = "Copied";
          setTimeout(function () { btn.textContent = "Copy"; }, 1500);
        });
      } catch (e) { /* clipboard unavailable, e.g. file:// in some browsers */ }
    });
    pre.appendChild(btn);
  });

  var toggle = document.querySelector(".menu-toggle");
  if (toggle) {
    toggle.addEventListener("click", function () {
      document.querySelector(".sidebar").classList.toggle("open");
    });
  }
})();
