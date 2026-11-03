# idp-host image: one static binary on scratch. Built by deploy/build.sh from a binary compiled on
# the host (CGO_ENABLED=0), with --network none, so no base image or package index is needed.
# Runs as uid 10001; works with a read-only root.
FROM scratch
# No --chmod here: BuildKit applies it to the created parent directories too, and a 0644 /etc is not
# traversable by uid 10001. The build context carries the right modes instead (deploy/build.sh).
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --chmod=0755 idp-host /usr/local/bin/idp-host
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/idp-host"]
CMD ["serve"]
