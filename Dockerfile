# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /out/llm-gateway ./cmd/llm-gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/llm-gateway /llm-gateway
COPY config.example.json /etc/llm-gateway/config.json
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/llm-gateway"]
CMD ["-config", "/etc/llm-gateway/config.json"]
