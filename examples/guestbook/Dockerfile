FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /guestbook .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /guestbook /guestbook
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/guestbook"]
