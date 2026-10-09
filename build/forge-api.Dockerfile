# Used by GoReleaser, which builds the binary first. For local images use the
# Dockerfile at the repo root.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETPLATFORM
ARG TARGETARCH
COPY ${TARGETPLATFORM}/forge-api /forge-api
COPY ${TARGETPLATFORM}/forge /cli/forge-linux-${TARGETARCH}
ENV FORGE_CLI_DIR=/cli
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/forge-api"]
