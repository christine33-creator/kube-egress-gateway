FROM golang:1.21-alpine AS builder

WORKDIR /app

COPY backend/go.mod ./
COPY backend/main.go ./
RUN go mod tidy
RUN go mod download

RUN CGO_ENABLED=0 GOOS=linux go build -o network-visualizer-backend main.go

FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /app/network-visualizer-backend .
COPY frontend /app/frontend

EXPOSE 8080

CMD ["./network-visualizer-backend"]
