FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
LABEL maintainer="jguzik@redhat.com"

ADD ops-proxy /usr/bin/ops-proxy
ENTRYPOINT ["/usr/bin/ops-proxy"]
