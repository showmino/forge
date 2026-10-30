# Proposed by Forge for this Go module. Forge runs it as a non-root user with
# a read-only root filesystem: only /tmp is writable. The app listens on $PORT.
FROM mirror.gcr.io/library/golang:{% .GoVersion %} AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/app {% .Package %}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
