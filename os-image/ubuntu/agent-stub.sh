#!/bin/sh
set -eu

case "${1:-}" in
    serve)
        echo "kumabox-agent stub: real vsock agent is implemented in P3-08/P3-09" >&2
        exec sleep infinity
        ;;
    version|--version)
        echo "kumabox-agent stub"
        ;;
    *)
        echo "usage: kumabox-agent {serve|version}" >&2
        exit 2
        ;;
esac
