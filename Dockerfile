FROM imqs/ubuntu-base
COPY migrator /opt
ENTRYPOINT ["/opt/migrator"]
