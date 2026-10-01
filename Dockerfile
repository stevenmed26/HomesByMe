FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /market ./cmd/market
FROM alpine:3.23
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /market /app/market
COPY migrations ./migrations
USER 65534:65534
ENTRYPOINT ["/app/market"]
CMD ["serve"]
