# The release workflow builds the binaries; the image only copies them.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETOS
ARG TARGETARCH
COPY dist/konmon-${TARGETOS}-${TARGETARCH} /konmon
USER 65532:65532
WORKDIR /data
ENTRYPOINT ["/konmon"]
CMD ["run", "--db", "/data/konmon.db"]
