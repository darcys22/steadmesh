# Browser variant of a seat image (access plugin "browser"): adds Chromium and
# a pinned Playwright MCP server, which `steadmesh-tools browser-mcp` runs
# headless with the system Chromium. Build on any node-based seat image:
#   docker build --build-arg BASE=steadmesh/seat-pi:dev -t steadmesh/seat-pi-browser:dev - < build/seat-browser.Dockerfile
ARG BASE
FROM ${BASE}
ARG PLAYWRIGHT_MCP_VERSION=0.0.83
USER root
# --ignore-scripts: Playwright's browser download is skipped; Debian's
# Chromium is used through --executable-path.
RUN apt-get update && apt-get install -y --no-install-recommends chromium fonts-liberation && \
    rm -rf /var/lib/apt/lists/* && \
    npm install -g --omit=dev --ignore-scripts "@playwright/mcp@${PLAYWRIGHT_MCP_VERSION}" && \
    npm cache clean --force && rm -rf /root/.npm /tmp/* && \
    test -x /usr/bin/chromium && command -v playwright-mcp && \
    v="$(/usr/bin/chromium --version)" && echo "chromium: $v"
ENV STEADMESH_BROWSER_EXECUTABLE=/usr/bin/chromium
USER 1000:1000
