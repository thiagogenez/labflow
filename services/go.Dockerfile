# Builds any Go service: docker build -f services/go.Dockerfile --build-arg SERVICE=payment-api .
# The build context is the repository root, so the service can use libs/go.

FROM golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS build
ARG SERVICE
WORKDIR /src
COPY libs/go/go.mod libs/go/go.sum libs/go/
COPY services/${SERVICE}/go.mod services/${SERVICE}/go.sum services/${SERVICE}/
WORKDIR /src/services/${SERVICE}
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY libs/go /src/libs/go
COPY services/${SERVICE} /src/services/${SERVICE}
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/service .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
COPY --from=build /out/service /service
# 65532 is the distroless "nonroot" user; numeric, so Kubernetes can verify runAsNonRoot.
USER 65532:65532
ENTRYPOINT ["/service"]
CMD ["serve"]
