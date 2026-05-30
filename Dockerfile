ARG GOLANG_VERSION="1.26"

FROM golang:${GOLANG_VERSION}-bookworm AS build-stage

# CI overrides these; defaults exist for local `docker build`.
ARG VERSION="v0.0.0"
ARG COMMIT_HASH="0000000"
ARG BUILD_TIMESTAMP="1970-01-01T00:00:00Z"
ARG PROJECT_URL="https://github.com/babs/ecr-prime"
ARG BUILDER="docker"

WORKDIR /app
COPY . .

RUN set -xe \
    && CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w \
            -X 'main.Version=${VERSION}' \
            -X 'main.CommitHash=${COMMIT_HASH}' \
            -X 'main.BuildTimestamp=${BUILD_TIMESTAMP}' \
            -X 'main.Builder=${BUILDER}' \
            -X 'main.ProjectURL=${PROJECT_URL}'" \
        -o /ecr-prime .

# distroless static: ships CA certs (needed for HTTPS to ECR) and runs non-root.
FROM gcr.io/distroless/static:nonroot AS final-stage

ARG VERSION COMMIT_HASH BUILD_TIMESTAMP PROJECT_URL
LABEL org.opencontainers.image.source=${PROJECT_URL}
LABEL org.opencontainers.image.created=${BUILD_TIMESTAMP}
LABEL org.opencontainers.image.version=${VERSION}
LABEL org.opencontainers.image.revision=${COMMIT_HASH}

COPY --from=build-stage /ecr-prime /usr/local/bin/ecr-prime
ENTRYPOINT ["/usr/local/bin/ecr-prime"]
