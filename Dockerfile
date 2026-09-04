# docker build -t imqs/migrator:latest .

# NOTE!
# This is never intended to be run as a container. It's meaningful output is a named
# container (imqs/migrator), which is used by the docker build of imqs/migrations.
# The migrations docker build just copies our binary "migrator" out of this container,
# and into it's own container.

FROM golang:1.22

# Authorize SSH Host
RUN mkdir -p ~/.ssh && \
	chmod 0700 ~/.ssh && \
	ssh-keyscan github.com > ~/.ssh/known_hosts

RUN --mount=type=ssh \
	git config --global url."git@github.com:".insteadOf "https://github.com/"

WORKDIR /build
COPY go.mod go.sum /build/
RUN --mount=type=ssh \
	go mod download

COPY . /build
RUN go build

FROM imqs/ubuntu-base:24.04

COPY --from=0 /build/migrator /opt/migrator

HEALTHCHECK CMD curl --fail http://localhost/ping || exit 1

ENTRYPOINT ["wait-for-nc.sh", "config:80", "--", "wait-for-postgres.sh", "db", "/opt/migrator"]
CMD ["serve", "80"]
