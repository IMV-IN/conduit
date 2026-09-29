# syntax=docker/dockerfile:1.7
# ---- UI build (embedded into the binary; React app lands in Phase 5) ----
FROM node:22-alpine AS ui
WORKDIR /ui
COPY ui/package*.json ./
RUN if [ -f package.json ]; then npm ci; fi
COPY ui/ ./
RUN if [ -f package.json ]; then npm run build; else mkdir -p dist && echo '<h1>Conduit</h1>' > dist/index.html; fi

# ---- Go build (static, CGO-free) ----
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
COPY --from=ui /ui/dist ./internal/admin/ui_dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/conduit ./cmd/conduit
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/mockprovider ./test/mockprovider

# ---- Runtime: conduit ----
FROM gcr.io/distroless/static-debian12:nonroot AS conduit
COPY --from=build /out/conduit /conduit
COPY conduit.example.yaml /etc/conduit/conduit.yaml
VOLUME ["/data"]
EXPOSE 8080 8081
USER nonroot:nonroot
ENTRYPOINT ["/conduit"]
CMD ["serve", "--config", "/etc/conduit/conduit.yaml"]

# ---- Runtime: mock provider (test only) ----
FROM gcr.io/distroless/static-debian12:nonroot AS mock
COPY --from=build /out/mockprovider /mockprovider
EXPOSE 9001
ENTRYPOINT ["/mockprovider", "-addr", ":9001"]
