FROM golang:1.25 AS builder

ARG APP_VERSION
RUN test -n "${APP_VERSION}"

WORKDIR /app

COPY go.* .
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
  CGO_ENABLED=0 go build -ldflags="-X meoe.io/cgroup-burst/internal/appconfig.externalVersion=${APP_VERSION}"

FROM alpine AS runtime

WORKDIR /
COPY --from=builder /app/cgroup-burst /cgroup-burst

RUN chmod +x /cgroup-burst

CMD ["/cgroup-burst"]
