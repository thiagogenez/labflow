# Builds the migrations image of a service whose own image holds no Go (D57):
# docker build -f services/migrations.Dockerfile --build-arg SERVICE=email-worker .
# It holds libs/go's migrate command and the service's migrations directory,
# one subdirectory per set of goose SQL files.

FROM golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS build
WORKDIR /src/libs/go
COPY libs/go/go.mod libs/go/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY libs/go ./
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
ARG SERVICE
ENV SERVICE=${SERVICE}
COPY --from=build /out/migrate /migrate
COPY services/${SERVICE}/migrations /migrations
# 65532 is the distroless "nonroot" user; numeric, so Kubernetes can verify runAsNonRoot.
USER 65532:65532
ENTRYPOINT ["/migrate"]
