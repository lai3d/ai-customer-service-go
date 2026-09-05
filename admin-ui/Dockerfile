# The operations UI: built here, served by nginx, deployed on its own.
#
# It is a separate image from the Go service on purpose. The two are separate
# applications on separate origins, which is what makes the CORS allowlist in
# internal/admin/cors.go a real control rather than a formality.
FROM node:22-alpine AS build
WORKDIR /app

# Dependencies first, so a source change does not reinstall them.
COPY package.json package-lock.json ./
RUN npm ci

COPY tsconfig.json vite.config.ts index.html ./
COPY public ./public
COPY src ./src
RUN npm run build

FROM nginxinc/nginx-unprivileged:1.27-alpine
COPY nginx.conf security-headers.conf /etc/nginx/conf.d/
RUN mv /etc/nginx/conf.d/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
COPY docker-entrypoint.sh /docker-entrypoint.d/99-admin-config.sh

# The image's own entrypoint runs everything in /docker-entrypoint.d first, so config.js
# is written before nginx starts.
USER 101
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
