FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /printmind-server main.go
RUN CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o /printmind-agent.exe ./cmd/agent/main.go

FROM alpine:3.20
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /printmind-server /app/printmind-server
COPY --from=builder /printmind-agent.exe /app/printmind-agent.exe
COPY data/db.json /app/data/db.json
EXPOSE 8090
CMD ["/app/printmind-server"]
