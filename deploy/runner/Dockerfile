# Image of the resource runner: forge-runner, OpenTofu, the Kubernetes
# provider and the module. A run downloads nothing.
#   docker build -f deploy/runner/Dockerfile -t forge-runner:dev .
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/forge-runner ./cmd/forge-runner

FROM mirror.gcr.io/library/alpine:3.22 AS module
# OpenTofu's image refuses to be a build stage; only its binary is copied.
COPY --from=ghcr.io/opentofu/opentofu:1.12.7 /usr/local/bin/tofu /usr/local/bin/tofu
COPY deploy/runner/module /opt/forge/module
COPY deploy/runner/tofurc /opt/forge/tofurc
WORKDIR /opt/forge/module
# Fetch the provider once, into the mirror the runs use. Then prove, from the
# mirror alone, that the module is valid; the build fails if it is not.
RUN tofu providers mirror /opt/forge/providers \
 && TF_CLI_CONFIG_FILE=/opt/forge/tofurc tofu init -backend=false -input=false \
 && TF_CLI_CONFIG_FILE=/opt/forge/tofurc tofu validate \
 && rm -rf .terraform

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=module /usr/local/bin/tofu /usr/local/bin/tofu
COPY --from=module /opt/forge /opt/forge
COPY --from=build /out/forge-runner /forge-runner
ENV TF_CLI_CONFIG_FILE=/opt/forge/tofurc \
    FORGE_MODULE_DIR=/opt/forge/module \
    FORGE_TOFU=/usr/local/bin/tofu
USER nonroot:nonroot
ENTRYPOINT ["/forge-runner"]
