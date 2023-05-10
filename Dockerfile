# syntax=docker/dockerfile:1

FROM golang:1.20

ENV PORT 9090

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . ./

RUN CGO_ENABLED=0 GOOS=linux go build -o /k3s-router

EXPOSE ${PORT}

CMD /k3s-router -p ${PORT}