# Maintaining the website

The site at https://steadmesh.com is the static `docs/` directory, published
by GitHub Pages from `main` (branch publishing, `/docs`). There is no build
step; `docs/.nojekyll` turns off Jekyll.

## Preview and check

```
make site-preview   # http://localhost:8000/
make site-check     # shared blocks, links, fragments, excerpts
```

Test under a path prefix too (links must be relative): serve the repository's
parent directory and open `/steadmesh/docs/`-style paths, or symlink `docs`
into an empty directory as `steadmesh`.

## Shared header, sidebar, pager, footer and metadata

Every page carries blocks between `<!-- shell:NAME -->` and
`<!-- /shell:NAME -->` markers. Never edit them by hand: change `NAV`,
`PAGES` or the block functions in `hack/site.py`, then run
`hack/site.py sync`. Sync also writes `docs/sitemap.xml` and the same blocks
into the demo page template in `tests/e2e/demo_test.go`.

To add a page: create it with the markers (copy `docs/faq.html`), add it to
`NAV` (or `PAGES` if it is not in the sidebar), and sync.

## Writing and examples

Lead with what someone can do with a team: a useful first task, the roles it
needs and the next step to try. Use plain, direct language in guides and
reference pages. Keep deployment, credential, retention and compatibility
warnings next to the actions they affect.

The homepage introduces possible workflows, not performance claims. Label
illustrative teams and suggested prompts as examples; do not present them as
recorded conversations or completed results. Link to the run record and
status page for test details, keeping pass/skip tables and benchmark-style
numbers off the landing page. Preserve recorded results when changing copy.

## Demo results

`docs/demo-results.html` and `docs/demo/*.json` are the demo's evidence,
written by `make demo` (`tests/e2e/demo_test.go`). Do not edit them by hand.
After a shell change, re-render the page from the recorded runs without a
cluster:

```
go test -count=1 -tags e2e -run '^TestDemoPage$' ./tests/e2e/
```

`docs/demo/pr-1.html` is a redacted, maintainer-reviewed summary of the pull
request from the recorded run (the demo repository is private). A new live
run needs a new reviewed summary before its link is meaningful.

## Versions

`hack/set-version.sh` (run by `make release`) pins quickstart/, the provider
docs, and the `?ref=` install commands in `docs/*.html` and `README.md`.
`hack/site.py sync` then updates the sidebar's "Documentation for vX" label
and the homepage's version and source links. The homepage excerpt
(`data-source`/`data-lines`) must match the checked-in file; `site-check`
fails when it drifts.

## Domain

`steadmesh.com` is set in the repository's Pages settings and DNS is managed
by the maintainer. Canonical and social URLs use `https://steadmesh.com/`
(`ORIGIN` in `hack/site.py`). `docs/404.html` uses absolute URLs because it
is served at any depth.
