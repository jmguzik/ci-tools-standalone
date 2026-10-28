FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

COPY ops-alert-dashboard /usr/bin/ops-alert-dashboard
USER 1001
EXPOSE 8080
ENTRYPOINT ["/usr/bin/ops-alert-dashboard"]
CMD ["--listen=127.0.0.1:8080"]
