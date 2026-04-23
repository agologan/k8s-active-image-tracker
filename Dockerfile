FROM golang:1.26 AS build
ARG GOARCH
ARG LDFLAGS_FLAG
ARG TAGS_FLAG

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/k8s-active-image-tracker-$GOARCH ./cmd/k8s-active-image-tracker $LDFLAGS_FLAG $TAGS_FLAG

FROM gcr.io/distroless/static:nonroot
ARG GOARCH
COPY --from=build /out/k8s-active-image-tracker-$GOARCH /k8s-active-image-tracker

WORKDIR /
ENTRYPOINT ["/k8s-active-image-tracker"]
