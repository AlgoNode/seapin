FROM golang:1.27.1-alpine3.24 AS build

RUN apk add --no-cache ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/ ./cmd/...

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/seapin /out/seapin-scan /
USER 1000:1000
ENTRYPOINT ["/seapin"]
CMD ["serve"]
