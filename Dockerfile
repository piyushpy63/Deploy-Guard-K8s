FROM golang:alpine AS builder

WORKDIR /app

# Copy module files first (better Docker layer caching)
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build the binary statically (pure Go driver modernc.org/sqlite does not require CGO)
RUN CGO_ENABLED=0 GOOS=linux go build -o deploy-guard cmd/guard/main.go

# Stage 2: final runtime image
FROM alpine:3.18 

# Install kubectl + sqlite3 CLI (for debugging)
RUN apk add --no-cache curl sqlite && \
    curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" && \
    chmod +x kubectl && \
    mv kubectl /usr/local/bin/kubectl

COPY --from=builder /app/deploy-guard /usr/local/bin/deploy-guard

RUN mkdir -p /data

ENTRYPOINT ["deploy-guard"]
