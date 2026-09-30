#!/bin/sh
# Submit the signed release binaries to Apple's notarization service and wait
# for the verdict.
#
# A plain binary cannot be stapled — stapling needs an app, dmg, or pkg — so
# the submission is a zip of the binaries. The ticket is bound to each binary's
# cdhash, so Gatekeeper accepts the binaries from the tarball the release ships:
# the container does not carry the notarization, the signed code does.
#
# Environment:
#   APPLE_API_KEY       base64 of the App Store Connect API .p8 private key
#   APPLE_API_KEY_ID    the key's id
#   APPLE_API_ISSUER    the issuer id (team's App Store Connect issuer)
set -eu

bin_dir=${1:?usage: notarize-macos.sh <binary directory>}

: "${APPLE_API_KEY:?base64 of the App Store Connect .p8 key}"
: "${APPLE_API_KEY_ID:?the App Store Connect key id}"
: "${APPLE_API_ISSUER:?the App Store Connect issuer id}"

key_file=$(mktemp)
zip_file=$(mktemp -d)/hive.zip
cleanup() { rm -f "$key_file"; }
trap cleanup EXIT

echo "$APPLE_API_KEY" | base64 --decode >"$key_file"

# -j flattens the paths: the submission is a manifest of binaries, not a
# directory layout.
zip -q -j "$zip_file" "$bin_dir"/hive "$bin_dir"/hive-plugin-*

output=$(xcrun notarytool submit "$zip_file" \
	--key "$key_file" --key-id "$APPLE_API_KEY_ID" --issuer "$APPLE_API_ISSUER" \
	--wait 2>&1) || {
	echo "$output"
	# A rejection without the log is unactionable: fetch it while the key
	# still exists.
	submission=$(echo "$output" | sed -n 's/^ *id: \([0-9a-f-]*\).*/\1/p' | head -n 1)
	if [ -n "$submission" ]; then
		xcrun notarytool log "$submission" \
			--key "$key_file" --key-id "$APPLE_API_KEY_ID" --issuer "$APPLE_API_ISSUER" || true
	fi
	exit 1
}
echo "$output"
