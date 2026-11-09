#!/bin/sh
# Substitute FOREVERBULL_WEB_URL into the provisioned dashboards, then start Grafana.
# set -e stops this script when apply-web-links exits non-zero, so /run.sh
# does not start Grafana after a bad web origin.
set -eu
/usr/local/bin/apply-web-links
exec /run.sh "$@"
