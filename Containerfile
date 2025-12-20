FROM golang:1.25-alpine as builder
WORKDIR /app
COPY . .

RUN go build .

FROM alpine:latest

COPY --from=builder --chmod=775 /app/transmission-natpmp-client /usr/bin/transmission-natpmp-client

ENTRYPOINT [ "transmission-natpmp-client" ]
