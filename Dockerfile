# 构建阶段
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download 2>/dev/null || true
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/autoclaw2api ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/autoclaw-login ./cmd/login && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/autoclaw-credit ./cmd/credit && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/autoclaw-maintain ./cmd/maintain

# 运行阶段
FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl bash python3
WORKDIR /app
COPY --from=build /out/autoclaw2api /out/autoclaw-login /out/autoclaw-credit /out/autoclaw-maintain /usr/local/bin/
COPY config.json /app/config.json
COPY login.sh credit.sh maintain.sh /app/
RUN chmod +x /app/*.sh
VOLUME ["/app/auths", "/app/data"]
EXPOSE 7865
CMD ["/app/autoclaw2api", "-config", "/app/config.json"]