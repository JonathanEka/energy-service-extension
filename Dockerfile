# Copyright (c) 2025 AccelByte Inc. All Rights Reserved.
# This is licensed software from AccelByte Inc, for limitations
# and restrictions contact your company contract manager.

# ----------------------------------------
# Stage 1: Protoc Code Generation
# ----------------------------------------
FROM --platform=$BUILDPLATFORM ubuntu:22.04 AS proto-builder

# Avoid warnings by switching to noninteractive
ENV DEBIAN_FRONTEND=noninteractive

ARG PROTOC_VERSION=21.9
ARG GO_VERSION=1.23.6

# Configure apt and install packages
RUN apt-get update \
    && apt-get -y install --no-install-recommends \
    #
    # Install essential development tools
    build-essential \
    ca-certificates \
    git \
    unzip \
    wget \
    #
    # Detect architecture for downloads
    && ARCH_SUFFIX=$(case "$(uname -m)" in \
        x86_64) echo "x86_64" ;; \
        aarch64) echo "aarch_64" ;; \
        *) echo "x86_64" ;; \
       esac) \
    && GOARCH_SUFFIX=$(case "$(uname -m)" in \
        x86_64) echo "amd64" ;; \
        aarch64) echo "arm64" ;; \
        *) echo "amd64" ;; \
       esac) \
    #
    # Install Protocol Buffers compiler
    && wget -O protoc.zip https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-${ARCH_SUFFIX}.zip \
    && unzip protoc.zip -d /usr/local \
    && rm protoc.zip \
    && chmod +x /usr/local/bin/protoc \
    #
    # Install Go
    && wget -O go.tar.gz https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH_SUFFIX}.tar.gz \
    && tar -C /usr/local -xzf go.tar.gz \
    && rm go.tar.gz \
    #
    # Clean up
    && apt-get autoremove -y \
    && apt-get clean -y \
    && rm -rf /var/lib/apt/lists/*

# Set up Go environment
ENV GOROOT=/usr/local/go
ENV GOPATH=/go
ENV PATH=$GOPATH/bin:$GOROOT/bin:$PATH

# Install protoc Go tools and plugins
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@latest \
    && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest \
    && go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest \
    && go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2@latest

# Set working directory
WORKDIR /build

# Copy proto sources and generator script
COPY proto.sh .
COPY pkg/proto/ pkg/proto/

# Generate protobuf files.
RUN chmod +x proto.sh && \
    ./proto.sh

# ----------------------------------------
# Stage 2: gRPC Server Builder
# ----------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.23 AS builder

ARG TARGETOS
ARG TARGETARCH

ARG GOOS=$TARGETOS
ARG GOARCH=$TARGETARCH
ARG CGO_ENABLED=0

# Set working directory
WORKDIR /build

# Copy application code (go mod tidy will handle dependencies)
COPY . .

# Copy generated protobuf files from stage 1
COPY --from=proto-builder /build/pkg/pb pkg/pb

# Tidy modules and build the Go application binary
# GOTOOLCHAIN=auto allows downloading newer Go if dependencies require it
RUN GOTOOLCHAIN=auto go mod tidy && GOTOOLCHAIN=auto go build -v -modcacherw -o /output/$TARGETOS/$TARGETARCH/service .

# ----------------------------------------
# Stage 3: Runtime Container
# ----------------------------------------
FROM alpine:3.22

# Set the value for the target OS and architecture.
ARG TARGETOS
ARG TARGETARCH

# Set working directory.
WORKDIR /app
RUN mkdir -p gateway/apidocs

# Copy api docs and build
COPY --from=proto-builder /build/gateway/apidocs gateway/apidocs
COPY --from=builder /output/$TARGETOS/$TARGETARCH/service service
COPY third_party third_party

# Plugin Arch gRPC Server Port
EXPOSE 6565

# gRPC Gateway Port
EXPOSE 8000

# Prometheus /metrics Web Server Port
EXPOSE 8080

# Entrypoint
CMD [ "/app/service" ]