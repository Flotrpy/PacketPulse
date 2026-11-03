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
# The agent users run on their own computers, served at /v1/agent/download/.
RUN for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
      os=${t%/*}; arch=${t#*/}; ext=; [ "$os" = windows ] && ext=.exe; \
      GOOS=$os GOARCH=$arch go build -trimpath -ldflags="-s -w" -o /out/agents/ppagent-$os-$arch$ext ./cmd/ppagent; \
    done

# City-level IP geolocation (DB-IP Lite, CC BY 4.0, monthly) so router hops on
# real traces can be placed. Falls back to last month early in the month.
FROM golang:1.24-bookworm AS geoip
RUN mkdir -p /out && ym=$(date -u +%Y-%m) && pm=$(date -u -d "$(date -u +%Y-%m-15) -1 month" +%Y-%m) && \
    (curl -fsSL "https://download.db-ip.com/free/dbip-city-lite-$ym.mmdb.gz" -o /tmp/city.gz || \
     curl -fsSL "https://download.db-ip.com/free/dbip-city-lite-$pm.mmdb.gz" -o /tmp/city.gz) && \
    gunzip -c /tmp/city.gz > /out/dbip-city-lite.mmdb && rm /tmp/city.gz

FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=api /out/packetpulse /app/packetpulse
COPY --from=web /src/frontend/dist /app/web
COPY --from=api /out/agents /app/agents
COPY scripts/windows/start-ppagent.ps1 /app/agents/start-ppagent.ps1
COPY --from=geoip /out/dbip-city-lite.mmdb /app/geoip/dbip-city-lite.mmdb
COPY data/ /app/data/
COPY backend/schema/examples/*.journey.json /app/seed/
ENV PORT=8080
EXPOSE 8080
# Only real traces are shown. Add "-seed", "/app/seed/*.journey.json" to load
# the recorded example journey (Jio Mumbai -> AWS Frankfurt) at startup.
ENTRYPOINT ["/app/packetpulse", "-data-dir", "/app/data", "-web", "/app/web", "-agent-dir", "/app/agents", "-state-dir", "/tmp/packetpulse"]
