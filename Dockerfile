FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pedagio ./cmd/pedagio

# distroless: sem shell, só o binário + certificados (o Bedrock é HTTPS).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pedagio /pedagio
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/pedagio"]
