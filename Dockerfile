# Stage 1: Build frontend
FROM node:22-alpine AS web-build
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: Build Go binary
FROM golang:1.24-alpine AS go-build
RUN apk add --no-cache gcc musl-dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-build /app/web/dist web/dist
RUN CGO_ENABLED=1 go build -o /forge ./cmd/forge

# Stage 3: Runtime
FROM alpine:3.21
RUN apk add --no-cache tmux git
COPY --from=go-build /forge /usr/local/bin/forge
ENTRYPOINT ["forge", "gateway"]
