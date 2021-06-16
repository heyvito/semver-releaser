FROM golang:1.16 AS build

RUN echo $(pwd)
RUN mkdir semver-releaser
WORKDIR /go/semver-releaser
COPY go.* main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o semver-releaser main.go

FROM alpine:3.10
COPY --from=build /go/semver-releaser/semver-releaser ./
ENTRYPOINT ["/semver-releaser"]
