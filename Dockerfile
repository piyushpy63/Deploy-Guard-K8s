FROM golang:1.25-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /app

# Copy module files first (better Docker layer caching)
COPY go.mod go.sum ./
RUN go mod download

COPY . .

#build the binary

RUN CGO_ENABLED=1 GOOS=linux go build -o deploy-guard cmd/guard/main.go


#stage 2

FROM alpine:3.18 

# Install kubectl + sqlite3 CLI (for debugging)
RUN apk add --no-cache curl sqlite && \
    curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" && \
    chmod +x kubectl && \
    mv kubectl /usr/local/bin/kubectl


COPY --from=builder /app/deploy-guard /usr/local/bin/deploy-guard

RUN mkdir -p /data

ENTRYPOINT ["deploy-guard"]



