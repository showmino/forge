# Image for forge-api. It also carries the forge CLI for linux, which forge-api serves to CI jobs at /cli/.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/showmino/forge/internal/version.Version=${VERSION} -X github.com/showmino/forge/internal/version.Commit=${COMMIT}" \
      -o /out/forge-api ./cmd/forge-api
RUN for arch in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
        -ldflags "-s -w -X github.com/showmino/forge/internal/version.Version=${VERSION} -X github.com/showmino/forge/internal/version.Commit=${COMMIT}" \
        -o /out/cli/forge-linux-$arch ./cmd/forge; \
    done

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/forge-api /forge-api
COPY --from=build /out/cli /cli
ENV FORGE_CLI_DIR=/cli
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/forge-api"]
