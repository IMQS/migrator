# NOTE!
# This is never intended to be run as a container. It's meaningful output is a named
# container (imqs/migrator), which is used by the docker build of imqs/migrations.
# The migrations docker build just copies our binary "migrator" out of this container,
# and into it's own container.

FROM golang:1.10
WORKDIR /build
RUN go get -u golang.org/x/vgo
COPY go.mod /build
COPY main.go /build
RUN CGO_ENABLED=0 GOOS=linux vgo build

FROM imqs/ubuntu-base
COPY --from=0 /build/migrator /opt/migrator
ENTRYPOINT ["wait-for-nc.sh", "config:80", "--", "wait-for-postgres.sh", "db", "/opt/migrator"]
CMD ["serve", "80"]