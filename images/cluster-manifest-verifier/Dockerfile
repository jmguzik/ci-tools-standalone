FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
LABEL maintainer="nmoraiti@redhat.com"

RUN microdnf install -y git && microdnf clean all && rm -rf /var/cache/dnf

ADD cluster-manifest-verifier /usr/bin/cluster-manifest-verifier

ENTRYPOINT ["/usr/bin/cluster-manifest-verifier"]
