FROM golang:1.27.3 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dirty
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /warc .

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=build /warc /usr/local/bin/warc
WORKDIR /home/nonroot
ENTRYPOINT ["/usr/local/bin/warc"]
CMD ["--help"]
