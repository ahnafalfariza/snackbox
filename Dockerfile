FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /snackbox ./cmd/snackbox

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /snackbox /snackbox
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/snackbox"]
