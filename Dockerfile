FROM golang:1.24-bookworm AS build

WORKDIR /src

COPY go.mod ./
COPY internal ./internal
COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/claude-openai-converter .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/claude-openai-converter /claude-openai-converter

EXPOSE 8787

ENV LISTEN_ADDR=:8787

ENTRYPOINT ["/claude-openai-converter"]
