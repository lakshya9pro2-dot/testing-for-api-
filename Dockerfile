# Render's native Go runtime is the recommended deployment path for this project.
# This Dockerfile is intentionally kept as an optional local/container fallback.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/watch-progress-api .

FROM alpine:3.22
RUN adduser -D -H -u 10001 app
WORKDIR /app
COPY --from=build /out/watch-progress-api ./watch-progress-api
COPY index.html ./index.html
USER app
EXPOSE 10000
CMD ["./watch-progress-api"]
