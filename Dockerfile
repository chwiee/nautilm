FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nautilm ./cmd/nautilm

# distroless: no shell, only the binary + certificates (Bedrock is HTTPS).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/nautilm /nautilm
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/nautilm"]
