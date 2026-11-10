import os

# gRPC's c-ares resolver ignores /etc/hosts. The TLS check points a test
# hostname at the Traefik port-forward, so this process uses the system
# resolver. localhost still resolves for the rest of the demo.
os.environ.setdefault("GRPC_DNS_RESOLVER", "native")
