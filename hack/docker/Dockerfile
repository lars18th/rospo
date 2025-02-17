# go backend builder
FROM golang:1.20 as gobuilder
ARG VERSION=development
WORKDIR /go/src/app
COPY . .
RUN go build \
    -trimpath \
    -ldflags="-s -w -X 'github.com/ferama/rospo/cmd.Version=$VERSION'" \
    -o /rospo .

# Final docker image
FROM ubuntu:latest
RUN set -eux; \
    apt update && \
    apt install -y \
        ca-certificates \
        sudo \
        git \
        curl \
        vim \
        byobu \
        psmisc \
        iputils-ping \
        netcat \
        dnsutils \
        bash-completion \
    && \
    apt clean

RUN \
    sed -i 's/^#force_color_prompt=yes/force_color_prompt=yes/' /etc/skel/.bashrc \
    && adduser --gecos "" --disabled-password rospo \
    && echo "rospo ALL=(ALL) NOPASSWD: ALL" >> /etc/sudoers \
    && addgroup rospo sudo

COPY --from=gobuilder /rospo /usr/local/bin/rospo

COPY ./hack/docker/entrypoint.sh /
ENTRYPOINT ["/entrypoint.sh"]