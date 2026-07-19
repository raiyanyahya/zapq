FROM golang:1.26-alpine AS builder
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /zapq . \
    && mkdir -p /data

# scratch: zero packages, zero CVE surface. The binary is fully static and
# doubles as its own healthcheck probe (no shell or wget needed).
FROM scratch
COPY --from=builder /zapq /usr/local/bin/zapq
COPY --from=builder --chown=10001:10001 /data /data
USER 10001:10001
WORKDIR /data
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
    CMD ["/usr/local/bin/zapq", "--healthcheck"]
ENTRYPOINT ["/usr/local/bin/zapq"]
CMD ["--data-dir", "/data"]
