# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /squamish-events ./cmd/server

# Runtime stage
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

COPY --from=builder /squamish-events /app/squamish-events

# Create data directory for SQLite
RUN mkdir -p /data

ENV PORT=8080
ENV DB_PATH=/data/squamish-events.db
ENV TZ=America/Vancouver

EXPOSE 8080

ENTRYPOINT ["/app/squamish-events"]
