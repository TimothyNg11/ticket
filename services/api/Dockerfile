# Build stage: full Go toolchain, discarded after build.
FROM golang:1.27-alpine AS build
WORKDIR /src
# Copy module files first so dependency download is cached across code changes.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./services/api

# Runtime stage: no shell, no package manager, runs as non-root (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --retries=3 CMD ["/api", "healthcheck"]
ENTRYPOINT ["/api"]
