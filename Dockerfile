FROM alpine:3.24
ARG TARGETPLATFORM
RUN apk add --no-cache bash ca-certificates curl git openssh-client procps-ng
COPY $TARGETPLATFORM/icebath /usr/bin/icebath
WORKDIR /work
ENTRYPOINT ["/usr/bin/icebath"]
