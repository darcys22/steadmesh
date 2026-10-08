#!/usr/bin/env python3
"""Keeps the static site in docs/ consistent and checks it.

  hack/site.py sync    rewrite the shared header, docs sidebar and footer of
                       every page (and the demo page template in
                       tests/e2e/demo_test.go) from the definitions below
  hack/site.py check   fail if any page's shared blocks are out of date, or if
                       an internal link, asset or fragment does not resolve

The shared blocks sit between <!-- shell:NAME --> and <!-- /shell:NAME -->
markers. Edit them here, never in the pages. The documented version comes
from quickstart/platform (pinned by hack/set-version.sh).

Standard library only; no dependency of the published site.
"""

import html
import re
import sys
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parent.parent
SITE = ROOT / "docs"
ORIGIN = "https://steadmesh.com/"
REPO = "https://github.com/darcys22/steadmesh"
DEMO_TEMPLATE = ROOT / "tests" / "e2e" / "demo_test.go"

# Documentation sidebar: (group, [(published path, label)]).
NAV = [
    ("Start", [
        ("docs/", "Overview"),
        ("quickstart.html", "Quickstart"),
        ("from-source.html", "Run from source"),
        ("tutorial.html", "Tutorial"),
        ("faq.html", "FAQ"),
    ]),
    ("Operate", [
        ("real-services.html", "Connect real services"),
        ("operations.html", "Operating an organisation"),
        ("sandbox.html", "Sandbox access"),
    ]),
    ("Reference", [
        ("configuration.html", "Configuration"),
        ("harnesses.html", "Harnesses and models"),
        ("tools.html", "Agent tools"),
        ("architecture.html", "Architecture"),
        ("decisions.html", "Design decisions"),
    ]),
    ("Evidence", [
        ("status.html", "Acceptance status"),
        ("demo-results.html", "Demo results"),
    ]),
]

# Every published page: path -> (kind, source file for "Edit this page").
# kind: "landing" (homepage), "page" (header + footer, no sidebar),
# "doc" (header + sidebar + footer),
# "error" (absolute links; served at any depth).
PAGES = {
    "index.html": ("landing", "docs/index.html"),
    "docs/index.html": ("doc", "docs/docs/index.html"),
    "faq.html": ("doc", "docs/faq.html"),
    "404.html": ("error", "docs/404.html"),
    "demo-results.html": ("doc", "tests/e2e/demo_test.go"),
    "demo/pr-1.html": ("page", "docs/demo/pr-1.html"),
}
for _, items in NAV:
    for path, _ in items:
        PAGES.setdefault("docs/index.html" if path == "docs/" else path, ("doc", "docs/" + path))


def version():
    text = (ROOT / "quickstart" / "platform" / "main.tf").read_text()
    m = re.search(r'\?ref=v([0-9.]+)', text)
    return m.group(1) if m else "main"


def prefix(page):
    return "../" * page.count("/")


def link(page, target):
    """href from page to a published path; the error page links absolutely."""
    if PAGES.get(page, ("",))[0] == "error":
        return ORIGIN + target
    p = prefix(page)
    return (p + target) if target else (p or "./")


def header(page):
    L = lambda t: html.escape(link(page, t))
    landing = PAGES[page][0] == "landing"
    home = "" if landing else L("")
    docs_current = ' aria-current="page"' if page.startswith("docs/") else ""
    faq_current = ' aria-current="page"' if page == "faq.html" else ""
    menu = ""
    if PAGES[page][0] == "doc":
        menu = ('\n  <button class="menu-toggle" type="button" aria-expanded="false" '
                'aria-controls="docs-nav">Docs menu</button>')
    return f"""<a class="skip" href="#content">Skip to content</a>
<header class="topbar">
  <a class="wordmark" href="{L('')}"><img src="{L('assets/favicon.svg')}" alt="" width="22" height="22">Steadmesh</a>{menu}
  <nav class="topnav" aria-label="Site">
    <a href="{home}#how-it-works">How it works</a>
    <a href="{home}#demo">Demo</a>
    <a href="{L('docs/')}"{docs_current}>Docs</a>
    <a href="{L('faq.html')}"{faq_current}>FAQ</a>
    <a href="{REPO}">GitHub</a>
    <a class="btn btn-small" href="{L('quickstart.html')}">Get started</a>
  </nav>
</header>"""


def sidebar(page):
    out = ['<aside class="sidebar">',
           f'  <nav id="docs-nav" aria-label="Documentation">',
           f'  <div class="brand-sub">Documentation for v{version()}</div>']
    for group, items in NAV:
        out.append(f'    <div class="nav-group">{group}</div>')
        out.append("    <ul>")
        for path, label in items:
            target = "docs/index.html" if path == "docs/" else path
            cur = ' aria-current="page"' if target == page else ""
            out.append(f'      <li><a href="{html.escape(link(page, path))}"{cur}>{label}</a></li>')
        out.append("    </ul>")
    out += ["  </nav>", "</aside>"]
    return "\n".join(out)


def footer(page):
    L = lambda t: html.escape(link(page, t))
    edit = ""
    src = PAGES[page][1]
    if PAGES[page][0] != "error":
        edit = (f'\n  <p class="edit"><a href="{REPO}/blob/main/{src}">Edit this page on GitHub</a>'
                f' (needs a GitHub account; changes go through a pull request).</p>')
    return f"""<footer class="sitefoot">
  <nav aria-label="Footer">
    <a href="{L('docs/')}">Documentation</a>
    <a href="{L('faq.html')}">FAQ</a>
    <a href="{REPO}">GitHub repository</a>
    <a href="{REPO}/releases">Releases</a>
    <a href="{REPO}/blob/main/LICENSE">License (Apache 2.0)</a>
    <a href="{L('status.html')}">Acceptance status</a>
    <a href="{L('demo-results.html')}">Demo results</a>
  </nav>{edit}
  <p>Steadmesh is open-source software. This site has no analytics or tracking.</p>
</footer>"""


def meta(page, text):
    """Canonical, icon and social metadata, from the page's own title and description."""
    title = re.search(r"<title>(.*?)</title>", text, re.S).group(1)
    desc = re.search(r'<meta name="description" content="([^"]*)">', text).group(1)
    url = ORIGIN + ("" if page == "index.html" else "docs/" if page == "docs/index.html" else page)
    lines = []
    if PAGES[page][0] != "error":
        lines.append(f'<link rel="canonical" href="{url}">')
    lines += [
        f'<link rel="icon" href="{html.escape(link(page, "assets/favicon.svg"))}" type="image/svg+xml">',
        f'<link rel="icon" href="{html.escape(link(page, "assets/favicon.png"))}" type="image/png" sizes="32x32">',
        '<meta name="theme-color" content="#10151f">',
        '<meta property="og:type" content="website">',
        '<meta property="og:site_name" content="Steadmesh">',
        f'<meta property="og:title" content="{title}">',
        f'<meta property="og:description" content="{desc}">',
        f'<meta property="og:image" content="{ORIGIN}assets/images/steadmesh-social.png">',
        '<meta property="og:image:width" content="1200">',
        '<meta property="og:image:height" content="630">',
        '<meta property="og:image:alt" content="Steadmesh: declare a persistent AI team in Terraform. A lead on Claude Code, an engineer on Codex and a reviewer on Pi.">',
        '<meta name="twitter:card" content="summary_large_image">',
    ]
    if PAGES[page][0] != "error":
        lines.insert(lines.index('<meta property="og:image:width" content="1200">') - 1, f'<meta property="og:url" content="{url}">')
    return "\n".join(lines)


def pager(page):
    order = [("docs/index.html" if p == "docs/" else p, label) for _, items in NAV for p, label in items]
    keys = [k for k, _ in order]
    i = keys.index(page)
    def a(j, rel):
        k, label = order[j]
        target = "docs/" if k == "docs/index.html" else k
        return f'<a class="{rel.lower()}" href="{html.escape(link(page, target))}" rel="{"prev" if rel == "Previous" else "next"}"><small>{rel}</small>{label}</a>'
    prev = a(i - 1, "Previous") if i > 0 else "<span></span>"
    nxt = a(i + 1, "Next") if i + 1 < len(order) else "<span></span>"
    return f'<nav class="pager" aria-label="Previous and next page">{prev}{nxt}</nav>'


BLOCKS = {"header": header, "sidebar": sidebar, "footer": footer, "meta": meta, "pager": pager}
MARK = re.compile(r"<!-- shell:(\w+) -->.*?<!-- /shell:\1 -->", re.S)


def render(text, page):
    def sub(m):
        name = m.group(1)
        body = BLOCKS[name](page, text) if name == "meta" else BLOCKS[name](page)
        return f"<!-- shell:{name} -->\n{body}\n<!-- /shell:{name} -->"
    text = text.replace('<html lang="en">', '<html lang="en" class="no-js">', 1)
    if page == "index.html":
        # The homepage always describes the current release.
        text = re.sub(r'<span class="ver">v[0-9.]+</span>', f'<span class="ver">v{version()}</span>', text)
        text = re.sub(r'(darcys22/steadmesh/blob/)v[0-9.]+/', rf'\g<1>v{version()}/', text)
    return MARK.sub(sub, text)


def targets():
    """(file, page key) pairs whose shell blocks are managed."""
    for page in PAGES:
        yield SITE / page, page
    yield DEMO_TEMPLATE, "demo-results.html"


def sitemap():
    urls = []
    for page, (kind, _) in PAGES.items():
        if kind == "error":
            continue
        urls.append(ORIGIN + ("" if page == "index.html" else "docs/" if page == "docs/index.html" else page))
    body = "".join(f"  <url><loc>{u}</loc></url>\n" for u in sorted(urls))
    return ('<?xml version="1.0" encoding="UTF-8"?>\n'
            '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n' + body + "</urlset>\n")


def sync(check_only):
    stale = []
    sm = SITE / "sitemap.xml"
    if not sm.exists() or sm.read_text() != sitemap():
        if check_only:
            stale.append("docs/sitemap.xml: out of date; run hack/site.py sync")
        else:
            sm.write_text(sitemap())
            print("updated docs/sitemap.xml")
    for f, page in targets():
        if not f.exists():
            stale.append(f"{f.relative_to(ROOT)}: missing")
            continue
        text = f.read_text()
        want = render(text, page)
        if want != text:
            if check_only:
                stale.append(f"{f.relative_to(ROOT)}: shared blocks out of date; run hack/site.py sync")
            else:
                f.write_text(want)
                print("updated", f.relative_to(ROOT))
    return stale


class Collect(HTMLParser):
    def __init__(self):
        super().__init__()
        self.ids, self.refs, self.dup, self.canonical, self.title = set(), [], [], None, False
        self.desc = False

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if "id" in a:
            if a["id"] in self.ids:
                self.dup.append(a["id"])
            self.ids.add(a["id"])
        if tag == "a" and "name" in a:
            self.ids.add(a["name"])
        for k in ("href", "src", "poster"):
            if k in a and a[k] is not None:
                self.refs.append((tag, k, a[k]))
        if "srcset" in a:
            for part in a["srcset"].split(","):
                self.refs.append((tag, "srcset", part.strip().split(" ")[0]))
        if tag == "link" and a.get("rel") == "canonical":
            self.canonical = a.get("href")
        if tag == "title":
            self.title = True
        if tag == "meta" and a.get("name") == "description" and a.get("content"):
            self.desc = True


def check_links():
    problems = []
    parsed = {}
    for f in sorted(SITE.rglob("*.html")):
        c = Collect()
        c.feed(f.read_text())
        parsed[f.resolve()] = c
    for f, c in parsed.items():
        rel = f.relative_to(SITE.resolve())
        page = rel.as_posix()
        for d in c.dup:
            problems.append(f"{rel}: duplicate id {d!r}")
        if not c.title or not c.desc:
            problems.append(f"{rel}: needs a <title> and a meta description")
        if page != "404.html" and (not c.canonical or not c.canonical.startswith(ORIGIN)):
            problems.append(f"{rel}: needs a canonical link under {ORIGIN}")
        for tag, attr, ref in c.refs:
            if ref == "":
                problems.append(f"{rel}: empty {attr} on <{tag}>")
                continue
            u = urlsplit(ref)
            if u.scheme in ("http", "https", "mailto"):
                if u.scheme == "http":
                    problems.append(f"{rel}: insecure link {ref}")
                if ref.startswith(ORIGIN) and page != "404.html" and tag == "a":
                    problems.append(f"{rel}: use a relative link instead of {ref}")
                continue
            if u.scheme:
                problems.append(f"{rel}: unexpected scheme in {ref}")
                continue
            if ref.startswith("/"):
                problems.append(f"{rel}: root-absolute {attr} {ref} breaks under a path prefix")
                continue
            target = (f.parent / unquote(u.path)).resolve() if u.path else f
            if u.path.endswith("/") or target.is_dir():
                target = target / "index.html"
            if not target.exists():
                problems.append(f"{rel}: {attr} {ref} does not resolve")
                continue
            if u.fragment and target.suffix == ".html":
                ids = parsed.get(target)
                if ids is not None and u.fragment not in ids.ids:
                    problems.append(f"{rel}: fragment #{u.fragment} not found in {target.relative_to(SITE.resolve())}")
    for css in SITE.rglob("*.css"):
        for ref in re.findall(r"url\(([^)]+)\)", css.read_text()):
            ref = ref.strip("'\"")
            if not ref.startswith(("data:", "#", "http")) and not (css.parent / ref).exists():
                problems.append(f"{css.relative_to(SITE)}: url({ref}) does not resolve")
    forbidden = [p for p in SITE.rglob("*") if p.is_file() and re.search(
        r"(\.tfstate|\.tfvars$|\.env$|kubeconfig|\.pem$|\.key$)", p.name)]
    problems += [f"{p.relative_to(SITE)}: must not be published" for p in forbidden]
    return sorted(set(problems))


def check_excerpts():
    """Code excerpts marked data-source/data-lines must match the checked-in file."""
    problems = []
    pat = re.compile(r'<pre[^>]*data-source="([^"]+)" data-lines="(\d+)-(\d+)"[^>]*><code>(.*?)</code></pre>', re.S)
    for f in SITE.rglob("*.html"):
        text = f.read_text()
        for src, a, b, code in pat.findall(text):
            want = "\n".join((ROOT / src).read_text().splitlines()[int(a) - 1:int(b)])
            if html.unescape(code) != want:
                problems.append(f"{f.relative_to(SITE)}: excerpt of {src}:{a}-{b} no longer matches the file")
            if f"blob/v{version()}/{src}" not in text:
                problems.append(f"{f.relative_to(SITE)}: link the excerpt's source at v{version()}")
    return problems


def main():
    cmd = sys.argv[1] if len(sys.argv) > 1 else "check"
    if cmd == "sync":
        for p in sync(False):
            print(p)
        return 0
    if cmd == "check":
        problems = sync(True) + check_links() + check_excerpts()
        for p in problems:
            print(p)
        print(f"{len(problems)} problem(s)" if problems else "site ok")
        return 1 if problems else 0
    print(__doc__)
    return 2


if __name__ == "__main__":
    sys.exit(main())
