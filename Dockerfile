# One multi-stage image recipe for every Go service. Pick the binary with
#   docker build --build-arg SERVICE=api|workers|payments_mock .
# or any main package with --build-arg PKG=./loadtest/onsale (load-test images).
# Build stage: full Go toolchain, discarded after build.
FROM golang:1.27-alpine AS build
ARG SERVICE=api
ARG PKG=./services/${SERVICE}
WORKDIR /src
# Copy module files first so dependency download is cached across code changes.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ${PKG}

# Runtime stage: no shell, no package manager, runs as non-root (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
