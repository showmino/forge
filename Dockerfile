# Image for forge-api. The CLI ships as binaries (GoReleaser), not as an image.
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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/forge-api /forge-api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/forge-api"]
