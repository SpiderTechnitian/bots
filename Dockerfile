FROM docker.io/library/golang:1.16-buster as build

ENV GO111MODULE=on
WORKDIR /go/src/trivia
ADD . /go/src/trivia

RUN go get -d -v ./...
RUN go build -o /go/bin/triviabot ./cmd/triviabot/

FROM gcr.io/distroless/base-debian10
COPY --from=build /go/bin/triviabot /
CMD ["/triviabot"]
