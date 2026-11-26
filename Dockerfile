# Server image: the server (/sandbox) and the in-sandbox snapshot tool (/sandbox-snap, copied into sandboxes by an init
# container). Binaries are built on the host (see Makefile) to keep the image tiny and the build fast.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETARCH
COPY bin/linux-${TARGETARCH}/sandbox bin/linux-${TARGETARCH}/sandbox-snap /
USER 65532:65532
ENTRYPOINT ["/sandbox"]
CMD ["serve"]
