#!/bin/sh
# With args (e.g. -c /path -r /path -g): run the bridge with those args.
# Without args: run with /data/config.yaml and /data/registration.yaml if the config exists;
# otherwise print an example config to stdout so first-run setup is easy.
if [ $# -eq 0 ]; then
	if [ -f /data/config.yaml ]; then
		exec /app/mrtc-sip-bridge -c /data/config.yaml -r /data/registration.yaml
	fi
	echo "No config found at /data/config.yaml. Outputting example config to stdout." >&2
	echo "Save this to a file, edit it, then run the container with the config and registration mounted at /data." >&2
	exec /app/mrtc-sip-bridge -e -c -
fi
exec /app/mrtc-sip-bridge "$@"
