# Build stage
FROM golang:1.24-bookworm AS build
WORKDIR /src

# CGO is required: github.com/mattn/go-sqlite3 is a cgo binding.
ENV CGO_ENABLED=1
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags="-s -w" -o /out/whatsapp-mcp-server ./cmd/server

# Runtime stage
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/whatsapp-mcp-server .
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["./whatsapp-mcp-server"]
