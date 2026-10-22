FROM scratch

USER 65534:65534

COPY --from=alpine:3.24@sha256:71e9b7bae160ba167fa17414401b94cbcdaf3c32feeab9298bd794a2a0614c35 /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/obsidibot /
ENTRYPOINT ["/obsidibot"]
