FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/fathom-front ./cmd/fathom-front

# The UI is compiled into the binary via embed, so nothing but the binary ships
# -- no asset directory to copy, and no writable layer for it to live on.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/fathom-front /usr/local/bin/fathom-front
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/fathom-front"]
