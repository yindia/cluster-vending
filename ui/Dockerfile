# syntax=docker/dockerfile:1
# Build context: ui/. Requires output: "standalone" in next.config.ts.
FROM node:24-bookworm-slim AS base
WORKDIR /app
ENV NEXT_TELEMETRY_DISABLED=1

FROM base AS dependencies
COPY package.json package-lock.json ./
RUN npm ci

FROM base AS builder
COPY --from=dependencies /app/node_modules ./node_modules
COPY . .
RUN npm run build && test -f .next/standalone/server.js

FROM base AS runner
ENV NODE_ENV=production \
    HOSTNAME=0.0.0.0 \
    PORT=3000
# The official Node image provides uid/gid 1000 as node.
COPY --from=builder --chown=node:node /app/.next/standalone ./
COPY --from=builder --chown=node:node /app/.next/static ./.next/static
COPY --from=builder --chown=node:node /app/public ./public
# The entrypoint in front of the standalone server (docker/serve.mjs): it
# makes same-origin API calls pass behind a port-forward or an Ingress.
COPY --chown=node:node docker/serve.mjs ./serve.mjs
USER node
EXPOSE 3000
CMD ["node", "serve.mjs"]
