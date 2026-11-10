FROM golang:1.27.4@sha256:08bcb99a38f85a265f9a93d1d3323e2440eec333380f3ea26ee0dec9264bb41c as build
WORKDIR /app/
COPY go.mod go.sum ./
COPY syntax/go.mod syntax/go.sum ./syntax/
RUN go mod download
COPY ./integration-tests/k8s/fixtures/otel-gen/ ./
RUN CGO_ENABLED=0 go build -o main main.go
FROM alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2
COPY --from=build /app/main /app/main
CMD ["/app/main"]
