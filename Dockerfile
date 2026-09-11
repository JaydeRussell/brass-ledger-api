# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src

# Copied and downloaded before the rest of the source so this layer is
# cached and skipped on every rebuild where go.mod/go.sum haven't
# changed — only real dependency changes trigger a re-download.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/server ./cmd/server

# Distroless: no shell, no package manager, nothing but the binary and
# the CA certificates it needs to reach a managed Postgres over TLS in
# production — about as small an attack surface as a container gets.
# ":nonroot" runs the process as an unprivileged user rather than root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
