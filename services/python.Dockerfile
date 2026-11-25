# Builds any Python service: docker build -f services/python.Dockerfile --build-arg SERVICE=email-worker .
# The build context is the repository root, so the service can use libs/python.
# Every dependency is a hash-checked wheel; only LabFlow's own packages are
# built, with the hash-checked build backend and no index.

FROM python:3.14.10-slim-trixie@sha256:22eafb370920ad8b429e0aec3fff433e0669968519f5d60469d83729c138b28b AS build
ARG SERVICE
ENV PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_CACHE_DIR=1
WORKDIR /src
RUN python -m venv /opt/build && python -m venv --without-pip /opt/venv
COPY requirements-build.txt ./
RUN /opt/build/bin/pip install --require-hashes --only-binary :all: -r requirements-build.txt
COPY services/${SERVICE}/requirements.txt services/${SERVICE}/
RUN /opt/build/bin/pip --python /opt/venv/bin/python install --require-hashes --only-binary :all: \
    -r services/${SERVICE}/requirements.txt
COPY libs/python libs/python
COPY services/${SERVICE} services/${SERVICE}
RUN /opt/build/bin/pip wheel --no-build-isolation --no-deps --no-index -w /wheels libs/python services/${SERVICE} \
    && /opt/build/bin/pip --python /opt/venv/bin/python install --no-deps --no-index /wheels/*.whl \
    && /opt/venv/bin/python -m compileall -q /opt/venv/lib

FROM python:3.14.10-slim-trixie@sha256:22eafb370920ad8b429e0aec3fff433e0669968519f5d60469d83729c138b28b
COPY --from=build /opt/venv /opt/venv
# Every Python service installs its command as `service`, so its image runs
# like a Go service's: /service serve, /service healthcheck.
RUN ln -s /opt/venv/bin/service /service
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
# 65532 is the same unprivileged user as the Go images; numeric, so Kubernetes can verify runAsNonRoot.
USER 65532:65532
ENTRYPOINT ["/service"]
CMD ["serve"]
