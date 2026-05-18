FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /proxy main.go

FROM alpine:3.21
COPY --from=build /proxy /proxy
EXPOSE 8080 8081
ENTRYPOINT ["/proxy"]
