# Build from the repository root: docker build -f examples/mock-entra/Dockerfile -t mock-entra:dev .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mock-entra ./examples/mock-entra

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mock-entra /mock-entra
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/mock-entra"]
