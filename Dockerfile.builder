# Shared builder stage: Go modules + bd + dolt.
# Cached independently — only rebuilds when go.mod/go.sum or versions change.
FROM golang:1.26-alpine AS deps

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

WORKDIR /src/operator
COPY operator/go.mod operator/go.sum ./
RUN go mod download

# Build bd from source (changes rarely).
ARG BEADS_VERSION=v1.0.2
WORKDIR /beads
RUN apk add --no-cache git \
    && git clone --depth 1 --branch "${BEADS_VERSION}" https://github.com/steveyegge/beads.git . \
    && CGO_ENABLED=0 GOOS=linux go build -o /out/bd ./cmd/bd/

# Install dolt (changes rarely).
FROM alpine:3.20 AS tools
ARG DOLT_VERSION=latest
RUN apk add --no-cache bash curl \
    && if [ "$DOLT_VERSION" = "latest" ]; then \
        curl -L https://github.com/dolthub/dolt/releases/latest/download/install.sh | bash; \
    else \
        curl -L "https://github.com/dolthub/dolt/releases/download/${DOLT_VERSION}/install.sh" | bash; \
    fi
