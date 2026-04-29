FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/router .

FROM alpine:3.20

RUN apk --no-cache add ca-certificates

COPY --from=builder /out/router /app/router

EXPOSE 8080
ENTRYPOINT ["/app/router"]
