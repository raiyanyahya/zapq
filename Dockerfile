FROM golang:1.22-alpine AS builder
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN go build -ldflags "-s -w -X main.version=${VERSION}" -o /zapq .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /zapq /usr/local/bin/zapq
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/zapq"]
