# Stage 1: Build Go binary
FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o storage-api .

# Stage 2: Runtime
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /app/storage-api .
ENV PORT=80
EXPOSE 80
CMD ["./storage-api"]
