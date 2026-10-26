# syntax=docker/dockerfile:1.7
# ──────────────────────────────────────────────────────────────────
# Stage 1: Build the React frontend
# ──────────────────────────────────────────────────────────────────
FROM node:22-alpine AS frontend-builder

WORKDIR /app/frontend

COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY frontend/ ./
RUN npm run build

# ──────────────────────────────────────────────────────────────────
# Stage 2: Build the Go backend (with the embedded frontend)
# ──────────────────────────────────────────────────────────────────
FROM golang:1.27.3-alpine AS backend-builder

RUN apk add --no-cache ca-certificates git && update-ca-certificates

WORKDIR /src

COPY backend ./
COPY .git /src/.git

# Drop the frontend build into the path the Go binary embeds via //go:embed
COPY --from=frontend-builder /app/frontend/dist ./cmd/single-user/frontend-dist/

# No -mod=vendor / GOPROXY=off here: Go uses a vendor/ tree when one is present
# (local checkouts, builds offline) and otherwise downloads modules (clean
# clone). Forcing -mod=vendor broke clean clones, which never have vendor/.
# Version/commit come from the copied git metadata; `--dirty` is omitted on
# purpose (only .git is copied, so --dirty would stamp every image "-dirty").
RUN chmod +x ./scripts/build.sh && \
    VERSION="$(git -C /src describe --tags --always | sed 's/^v//')" \
    COMMIT="$(git -C /src rev-parse --short HEAD)" \
    CGO_ENABLED=0 GOOS=linux ./scripts/build.sh /isola ./cmd/single-user

# ──────────────────────────────────────────────────────────────────
# Stage 3: Minimal runtime image
# ──────────────────────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata docker-cli wget

COPY --from=backend-builder /isola /isola
COPY --from=backend-builder /src/config.yaml /config.yaml
COPY browsers/isola-base/seccomp.json /etc/isola/seccomp.json

EXPOSE 8080

ENTRYPOINT ["/isola"]
