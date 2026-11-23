FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
LABEL maintainer="jguzik@redhat.com"

RUN microdnf install -y git && microdnf clean all && rm -rf /var/cache/dnf

ADD pipeline-controller /usr/bin/pipeline-controller
ENTRYPOINT ["/usr/bin/pipeline-controller"]
