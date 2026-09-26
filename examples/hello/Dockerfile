FROM golang:1-alpine AS build
WORKDIR /src
COPY main.go index.html ./
RUN CGO_ENABLED=0 go build -o /hello main.go

FROM alpine:3.22
COPY --from=build /hello /hello
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/hello"]
