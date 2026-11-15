# srvm image: the server (/srvm) and the in-sandbox snapshot tool (/srvm-snap, copied into sandboxes by an init
# container). Binaries are built on the host (see Makefile) to keep the image tiny and the build fast.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETARCH
COPY bin/linux-${TARGETARCH}/srvm bin/linux-${TARGETARCH}/srvm-snap /
USER 65532:65532
ENTRYPOINT ["/srvm"]
CMD ["serve"]
