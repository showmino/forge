# Used by GoReleaser, which builds the binary first. For local images use the
# Dockerfile at the repo root.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/forge-api /forge-api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/forge-api"]
