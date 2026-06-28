# syntax=docker/dockerfile:1
# Build the Lit SPA and serve it with unprivileged nginx. nginx also proxies
# /api, /auth, and the JWKS endpoint to the api service inside the cluster.
FROM node:22-bookworm AS build
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM nginxinc/nginx-unprivileged:1.27-alpine
COPY deploy/docker/nginx/default.conf /etc/nginx/conf.d/default.conf
COPY --from=build /web/dist /usr/share/nginx/html
EXPOSE 8080
