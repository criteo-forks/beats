FROM golang:1.18

# Install RPM development packages
RUN apt-get update && apt-get install -y rpm librpm-dev

ENV APP_DIR=/home/ci/beats
RUN groupadd ci && \
    useradd -s /bin/bash -m -g ci ci
RUN mkdir -p $APP_DIR
COPY --chown=ci:ci / $APP_DIR
WORKDIR $APP_DIR/x-pack/auditbeat/
RUN GOOS=linux GOARCH=amd64 go build .