FROM golang:1.26 AS build
ARG GOARCH
ARG LDFLAGS_FLAG
ARG TAGS_FLAG
ARG ECR_HELPER_VERSION=v0.10.1

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build $LDFLAGS_FLAG $TAGS_FLAG -o /out/k8s-active-image-tracker ./cmd/k8s-active-image-tracker
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH GOBIN=/out go install github.com/awslabs/amazon-ecr-credential-helper/ecr-login/cli/docker-credential-ecr-login@$ECR_HELPER_VERSION

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && \
    addgroup -S app && \
    adduser -S -D -h /home/app -G app app

COPY --from=build /out/k8s-active-image-tracker /usr/local/bin/k8s-active-image-tracker
COPY --from=build /out/docker-credential-ecr-login /usr/local/bin/docker-credential-ecr-login

ENV HOME=/home/app
USER app
WORKDIR /home/app
ENTRYPOINT ["/usr/local/bin/k8s-active-image-tracker"]
