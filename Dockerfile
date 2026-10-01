# One multi-stage image recipe for every Go service. Pick the binary with
#   docker build --build-arg SERVICE=api|workers|payments_mock .
# Build stage: full Go toolchain, discarded after build.
FROM golang:1.27-alpine AS build
ARG SERVICE=api
WORKDIR /src
# Copy module files first so dependency download is cached across code changes.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./services/${SERVICE}

# Runtime stage: no shell, no package manager, runs as non-root (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
