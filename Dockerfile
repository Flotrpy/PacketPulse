# All-in-one test deployment: ingestion + compliance engine + globe on one port.
#   docker build -t packetpulse .
#   docker run --rm -p 8080:8080 -e PACKETPULSE_ADMIN_TOKENS=me:<token> packetpulse
# Tracing needs raw ICMP sockets: the container runs as root, and Docker's
# default capability set includes NET_RAW. /healthz reports "tracing".

FROM node:22-bookworm-slim AS web
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json frontend/.npmrc ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
RUN npm run build

FROM golang:1.24-bookworm AS api
WORKDIR /src/backend
ENV GOTOOLCHAIN=local CGO_ENABLED=0
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN go build -trimpath -ldflags="-s -w" -o /out/packetpulse ./cmd/packetpulse

FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=api /out/packetpulse /app/packetpulse
COPY --from=web /src/frontend/dist /app/web
COPY data/ /app/data/
COPY backend/schema/examples/*.journey.json /app/seed/
ENV PORT=8080
EXPOSE 8080
# -seed loads the recorded example journey so the globe and alert stream are
# not empty before the first live trace; drop it for a clean start.
ENTRYPOINT ["/app/packetpulse", "-data-dir", "/app/data", "-web", "/app/web", "-state-dir", "/tmp/packetpulse", "-seed", "/app/seed/*.journey.json"]
