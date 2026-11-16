# Build:  docker build -t hello:1 examples/hello
FROM golang:1.24 AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -o /hello main.go

FROM scratch
COPY --from=build /hello /hello
EXPOSE 8080
ENTRYPOINT ["/hello"]
