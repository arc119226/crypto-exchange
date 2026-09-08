# syntax=docker/dockerfile:1.7
# The edge image for the beta VM (deploy/compose/compose.prod.yaml): the
# trade front end (web/trade) built once, served by Caddy with the
# Caddyfile next to this file. Nothing in this image knows a secret.
FROM node:22-bookworm-slim AS build
WORKDIR /web
ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
COPY web/trade/package.json web/trade/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web/trade/ ./
RUN npm run build

FROM caddy:2.10.2
COPY --from=build /web/dist /srv
COPY build/edge/Caddyfile /etc/caddy/Caddyfile
EXPOSE 80 443 443/udp
