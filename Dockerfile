FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
	-ldflags "-s -w -X sunfire/internal/version.Number=$VERSION" \
	-o /out/sunfired ./cmd/sunfired

FROM alpine:3.20

RUN apk add --no-cache ca-certificates

COPY --from=build /out/sunfired /usr/bin/sunfired

ENTRYPOINT ["/usr/bin/sunfired"]
CMD ["-config", "/etc/sunfire/main.conf"]
