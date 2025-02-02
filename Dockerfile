FROM golang:1.22-alpine3.19 as build

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./

ENV CGO_ENABLED=0
RUN go build -o /app/bin/app ./main.go

FROM scratch

WORKDIR /

COPY --from=build /app/bin/app /app

ENTRYPOINT [ "/app" ]