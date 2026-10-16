# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bin/pricing-engine ./cmd/pricing-engine && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bin/simulator ./cmd/simulator && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bin/wsbench ./cmd/wsbench && \
    mkdir -p /out/secrets

# Static binaries on distroless: no shell, no package manager, non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bin/ /usr/local/bin/
# Owned by nonroot so a fresh named volume mounted here is writable.
COPY --from=build --chown=nonroot:nonroot --chmod=700 /out/secrets /secrets
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/pricing-engine"]
