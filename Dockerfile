FROM node:22-alpine AS web-build

WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.23-alpine AS server-build

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /send-and-safe ./cmd/server

FROM alpine:3.21

RUN addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=server-build /send-and-safe /usr/local/bin/send-and-safe
COPY --from=web-build /app/web/dist ./web/dist

RUN mkdir -p /app/data && chown -R app:app /app
USER app

ENV ADDR=:8080
ENV DATA_DIR=/app/data
ENV WEB_DIR=/app/web/dist

EXPOSE 8080
VOLUME ["/app/data"]

ENTRYPOINT ["send-and-safe"]
