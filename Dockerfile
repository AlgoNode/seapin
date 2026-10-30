FROM golang:1.27.1-alpine3.24 AS build

RUN apk add --no-cache ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /seapin ./cmd/seapin

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /seapin /seapin
USER 1000:1000
ENTRYPOINT ["/seapin"]
CMD ["serve"]
