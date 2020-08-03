FROM golang:1.14-alpine AS builder
ENV GO111MODULE on
ENV CGO_ENABLED 0
WORKDIR /build
COPY . .
RUN go build -a -installsuffix cgo -o /usr/bin/entrypoint ./cmd/relay-entrypoint

FROM relaysh/core

COPY --from=builder /usr/bin/entrypoint /usr/bin/entrypoint
RUN apk --no-cache add ca-certificates && update-ca-certificates
ENTRYPOINT ["ni"]
