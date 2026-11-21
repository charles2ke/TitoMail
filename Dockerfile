# syntax=docker/dockerfile:1

# Build the web interface.
FROM node:24-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Build a static server binary with the web interface embedded.
FROM golang:1.26 AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY web/embed.go web/
COPY --from=web /src/web/dist/ web/dist/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/titomail ./cmd/titomail \
    && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=server /out/titomail /usr/local/bin/titomail
COPY --from=server --chown=65532:65532 /out/data /data
# Standard ports. HTTPS, IMAPS and SMTPS (465) need TLS, so they are
# enabled together with TITOMAIL_ACME or TITOMAIL_TLS_CERT (see
# docker-compose.yml). Docker lets non-root containers bind ports below 1024.
ENV TITOMAIL_DATA_DIR=/data \
    TITOMAIL_HTTP_ADDR=:80 \
    TITOMAIL_SMTP_ADDR=:25 \
    TITOMAIL_SUBMISSION_ADDR=:587 \
    TITOMAIL_IMAP_ADDR=:143
VOLUME /data
EXPOSE 25 80 143 443 465 587 993
ENTRYPOINT ["/usr/local/bin/titomail"]
CMD ["serve"]
